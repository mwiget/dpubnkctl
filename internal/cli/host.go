package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/mwiget/dpubnkctl/internal/poc"
	"github.com/mwiget/dpubnkctl/internal/ssh"
)

func newHostCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "host",
		Short: "Bare-metal host management (data-plane network setup, etc.)",
	}
	cmd.AddCommand(newHostNetworkCmd())
	return cmd
}

func newHostNetworkCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "network",
		Short: "Configure host-side network",
	}
	cmd.AddCommand(newHostNetworkSetupCmd())
	return cmd
}

type hostNetworkSetupFlags struct {
	pocDir         string
	yolo           bool
	confirmCluster string
	dryRun         bool
}

func newHostNetworkSetupCmd() *cobra.Command {
	f := &hostNetworkSetupFlags{}
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Add data-plane VLAN sub-interfaces on each host (DESTRUCTIVE — netplan apply)",
		Long: `For each host in poc.yaml with a data_plane block:

  1. Render a netplan YAML with one VLAN sub-interface per
     data_plane.vlans entry. Sub-interface names follow Role+Tag
     (e.g. external40, internal41) — matching the OVS port names
     on the DPU side.
  2. SCP it to /etc/netplan/70-dpubnkctl-dataplane.yaml on the host
     (mode 0600).
  3. Apply via "sudo netplan apply".
  4. Verify each sub-interface came up.

The data-plane network is the high-speed VLAN that k8s control-plane
traffic (apiserver, kubelet, Calico tunnels, east-west pod-to-pod)
traverses once cluster up runs with the matching inventory.

No bond on the host — bonding is handled inside the DPU's br-lag.
The host attaches all VLAN sub-ifs to a single PF (data_plane.parent_iface).

Required gates:
  --yolo                   acknowledge that this rewrites netplan
  --confirm-cluster NAME   must equal poc.yaml.metadata.name`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runHostNetworkSetup(cmd.Context(), cmd.OutOrStdout(), f)
		},
	}
	cmd.Flags().StringVar(&f.pocDir, "poc", "", "PoC repo path (default: current directory)")
	cmd.Flags().BoolVar(&f.yolo, "yolo", false, "Acknowledge netplan rewrite + apply")
	cmd.Flags().StringVar(&f.confirmCluster, "confirm-cluster", "", "Must equal poc.yaml.metadata.name (typo guard)")
	cmd.Flags().BoolVar(&f.dryRun, "dry-run", false, "Print rendered netplan without applying")
	return cmd
}

func runHostNetworkSetup(ctx context.Context, out io.Writer, f *hostNetworkSetupFlags) error {
	repo, err := resolvePoCDir(f.pocDir)
	if err != nil {
		return err
	}
	p, err := poc.Load(repo)
	if err != nil {
		return fmt.Errorf("not a PoC repo (%s): %w", repo, err)
	}
	if !f.dryRun {
		if err := requireTwoGates(f.yolo, "--confirm-cluster", f.confirmCluster, p.Metadata.Name, "netplan rewrite (use --dry-run to skip)"); err != nil {
			return err
		}
	}
	if err := enforceValidateForPhase(out, p, repo, poc.PhaseCluster, false); err != nil {
		return err
	}

	var jobs []*poc.Host
	for i := range p.Hosts {
		if p.Hosts[i].DataPlane != nil && len(p.Hosts[i].DataPlane.VLANs) > 0 {
			jobs = append(jobs, &p.Hosts[i])
		}
	}
	if len(jobs) == 0 {
		return errors.New("no hosts in poc.yaml have a data_plane.vlans entry — set host.data_plane.vlans on each host first")
	}

	fmt.Fprintf(out, "PoC:    %s\n", p.Metadata.Name)
	fmt.Fprintf(out, "Hosts:  %d host(s) with data_plane configured\n\n", len(jobs))

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		failures []string
	)

	for _, h := range jobs {
		h := h
		wg.Add(1)
		go func() {
			defer wg.Done()
			tag := fmt.Sprintf("[%s]", h.Name)
			err := setupOneHost(ctx, out, repo, h, p.Network.DPUMTU, f.dryRun, f.yolo, tag)
			if err != nil {
				mu.Lock()
				failures = append(failures, fmt.Sprintf("%s: %v", h.Name, err))
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(failures) > 0 {
		return fmt.Errorf("%d host(s) failed:\n  - %s", len(failures), strings.Join(failures, "\n  - "))
	}
	fmt.Fprintln(out, "\nDONE.")
	return nil
}

func setupOneHost(ctx context.Context, out io.Writer, repo string, h *poc.Host, defaultMTU int, dryRun, yolo bool, tag string) error {
	dp := h.DataPlane
	defMTU := dp.MTU
	if defMTU == 0 {
		defMTU = defaultMTU
	}
	if defMTU == 0 {
		defMTU = 9000
	}

	netplan := renderHostNetplan(dp.ParentIface, dp.VLANs, defMTU)
	fmt.Fprintf(out, "%s rendered netplan:\n%s\n", tag, indent(netplan, "  "))

	if dryRun {
		fmt.Fprintf(out, "%s --dry-run: not applying.\n", tag)
		return nil
	}

	cfg, err := sshConfigForHost(repo, h, 30*time.Second)
	if err != nil {
		return err
	}
	dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	c, err := ssh.Dial(dialCtx, cfg)
	cancel()
	if err != nil {
		return err
	}
	defer func() {
		if c != nil {
			c.Close()
		}
	}()

	// Pre-flight: catch the post-BFB "ghost PF" state before netplan
	// blunders into opaque RTNETLINK errors. A successful BFB flash
	// leaves the host's mlx5_core PF detached from the kernel — the
	// interface name exists, but `ethtool -i` returns "No such device".
	//
	// When --yolo is set, the last-resort path is to issue `sudo reboot`
	// (only a fresh boot reliably clears the ghost state on some setups)
	// and reconnect — see AGENTS.md #29 + #9. The reboot needs the same
	// SSH config + a new client after the host comes back, so pass them
	// in. Non-yolo runs keep the historical "reboot manually" error so
	// an interactive operator isn't surprised by a sudden 5-min reboot.
	if recovered, err := recoverGhostPF(ctx, c, cfg, dp.ParentIface, tag, yolo, out); err != nil {
		return err
	} else if recovered != nil {
		// Reconnect happened; close the old client (defer above runs at
		// function exit, but we now have a fresh client for everything
		// below) and adopt the new one.
		c.Close()
		c = recovered
	}

	const remotePath = "/etc/netplan/70-dpubnkctl-dataplane.yaml"
	tmpRemote := "/tmp/dpubnkctl-dataplane-netplan.yaml"

	fmt.Fprintf(out, "%s SCP → %s ...\n", tag, remotePath)
	if err := c.PushBytes(ctx, []byte(netplan), tmpRemote); err != nil {
		return fmt.Errorf("scp tmp: %w", err)
	}
	if r := c.Run(ctx, fmt.Sprintf("sudo -n install -m 0600 %s %s && sudo -n netplan apply 2>&1", tmpRemote, remotePath)); !r.OK() {
		return fmt.Errorf("netplan apply: exit=%d %s", r.ExitCode, strings.TrimSpace(r.Stderr+r.Stdout))
	}

	// Brief grace period for the kernel to bring up the sub-interfaces.
	time.Sleep(3 * time.Second)
	for _, v := range dp.VLANs {
		want := stripCIDR(v.IP)
		port := v.PortName()
		r := c.Run(ctx, fmt.Sprintf("ip -br addr show dev %s 2>&1", port))
		if !r.OK() || !strings.Contains(r.Stdout, want) {
			return fmt.Errorf("verify: %s does not show %s\n%s", port, want, r.Stdout)
		}
		fmt.Fprintf(out, "%s up: %s %s\n", tag, port, strings.TrimSpace(r.Stdout))
	}
	return nil
}

// renderHostNetplan produces netplan YAML with one VLAN sub-interface per
// HostDataPlaneVLAN, all anchored to the same parent PF. Sub-if names use
// Role+Tag (e.g. "internal41") so they line up 1:1 with the OVS port
// names on the DPU side. The parent interface itself is left to whatever
// existing netplan owns it — netplan resolves `link:` across all files.
// recoverGhostPF handles the post-flash "ghost mlx5_core PF" state on
// the host's data-plane parent interface. After a BFB flash the host
// side of the BlueField PCIe link can briefly lose its mlx5_core
// netdev — ethtool reports `No such device`. provision dpu's settle
// wait usually catches this within 10-30s; when it doesn't, this two-
// tier recovery kicks in:
//
//   tier 1   `modprobe -r mlx5_core; modprobe mlx5_core` + 90s poll.
//            Drives a clean kernel re-probe of every mlx5 PCIe device.
//            udev rename adds another ~5-30s after the kernel probe
//            completes — hence 90s, not 30s (the 30s ceiling in an
//            earlier fix hit the deadline 4s before udev renamed on
//            the May 16 homelab run).
//
//   tier 2   PCIe `remove` + `rescan` on every BlueField (vendor 15b3)
//            BDF, then another 60s poll. Per-device equivalent of
//            modprobe reload; works when the driver is stuck on one
//            specific function.
//
// We avoid suggesting a host reboot — on Proxmox VFIO-passthrough
// setups (BF3 attached at PCIe 82:00/c1:00 etc.) a guest reboot
// during the post-flash window can hang the host kernel's PCIe reset
// path. Verified on rome1, 2026-05-15. See AGENTS.md #29.
// recoverGhostPF returns (newClient, nil) when a reboot happened and a
// fresh SSH client took the place of `c`. The caller adopts the new
// client. Returns (nil, nil) on success without reboot; (nil, err) on
// hard failure. The yolo flag gates the reboot path: only --yolo runs
// will reboot the host autonomously; interactive runs still get the
// historical "reboot manually" error.
func recoverGhostPF(ctx context.Context, c *ssh.Client, cfg ssh.Config, parentIface, tag string, yolo bool, out io.Writer) (*ssh.Client, error) {
	ethtoolCheck := func() (bool, string) {
		r := c.Run(ctx, fmt.Sprintf("ethtool -i %s 2>&1", parentIface))
		combined := strings.TrimSpace(r.Stdout + r.Stderr)
		return r.OK(), combined
	}
	pollUntilLive := func(deadline time.Time) (bool, string) {
		iter := 0
		var ok bool
		var combined string
		for {
			time.Sleep(2 * time.Second)
			iter++
			ok, combined = ethtoolCheck()
			if ok && strings.Contains(combined, "mlx5_core") {
				fmt.Fprintf(out, "%s   %s live after %d polls.\n", tag, parentIface, iter)
				return true, combined
			}
			if time.Now().After(deadline) {
				return false, combined
			}
		}
	}

	ok, combined := ethtoolCheck()
	if ok && strings.Contains(combined, "mlx5_core") {
		return nil, nil
	}

	// Both recovery tiers run in sequence on the ghost-state path. We
	// don't early-return on tier-1 success — the final re-check at the
	// bottom is the single exit point that turns "ethtool now sees
	// mlx5_core" into a clean return. This keeps tier-1 and tier-2
	// flow-symmetric for readers.
	if !ok && strings.Contains(combined, "No such device") {
		fmt.Fprintf(out, "%s ghost mlx5_core PF detected on %s; attempting modprobe reload ...\n",
			tag, parentIface)
		if r := c.Run(ctx, "sudo -n bash -c 'modprobe -r mlx5_core 2>&1 || true; modprobe mlx5_core 2>&1'"); !r.OK() {
			fmt.Fprintf(out, "%s   modprobe reload exit=%d: %s\n",
				tag, r.ExitCode, strings.TrimSpace(r.Stdout+r.Stderr))
		}
		fmt.Fprintf(out, "%s   polling ethtool -i %s for up to 90s ...\n", tag, parentIface)
		recovered, _ := pollUntilLive(time.Now().Add(90 * time.Second))
		if recovered {
			fmt.Fprintf(out, "%s   mlx5_core %s recovered after modprobe reload.\n", tag, parentIface)
		} else {
			fmt.Fprintf(out, "%s   still ghost after modprobe + 90s; trying PCIe remove+rescan ...\n", tag)
			rescan := `sudo -n bash -c '
for bdf in $(lspci -d 15b3: | awk "{print \$1}"); do
  echo 1 | tee /sys/bus/pci/devices/0000:$bdf/remove >/dev/null 2>&1 || true
done
sleep 1
echo 1 | tee /sys/bus/pci/rescan >/dev/null'`
			if r := c.Run(ctx, rescan); !r.OK() {
				fmt.Fprintf(out, "%s   PCIe rescan exit=%d: %s\n",
					tag, r.ExitCode, strings.TrimSpace(r.Stdout+r.Stderr))
			}
			fmt.Fprintf(out, "%s   polling again for up to 60s ...\n", tag)
			if rescanOK, _ := pollUntilLive(time.Now().Add(60 * time.Second)); rescanOK {
				fmt.Fprintf(out, "%s   mlx5_core %s recovered after PCIe rescan.\n", tag, parentIface)
			}
		}
	}

	// Final re-check after recovery attempts. Single exit point so the
	// error message reflects the latest state regardless of which tier
	// (or no tier) actually fired.
	ok, combined = ethtoolCheck()
	if ok && strings.Contains(combined, "mlx5_core") {
		return nil, nil
	}
	if strings.Contains(combined, "No such device") || strings.TrimSpace(combined) == "" {
		if yolo {
			// Tier 3 (--yolo only): host reboot. Some setups (post-flash
			// homelab, May 16) leave stale netlink state that only a
			// fresh boot clears (AGENTS.md #9). On Proxmox VFIO setups
			// this risks hanging the hypervisor — but --yolo means the
			// operator owns the risk; the caller already passed the
			// `--confirm-cluster <name>` gate.
			return autoReboot(ctx, c, cfg, parentIface, tag, out)
		}
		return nil, fmt.Errorf("parent iface %s did not recover after modprobe reload + PCIe rescan — reboot the host manually (BF3 EMBEDDED_CPU rules out mlxfwreset). See AGENTS.md #29. Pass --yolo to let the tool reboot for you", parentIface)
	}
	return nil, fmt.Errorf("ethtool -i %s failed: %s", parentIface, combined)
}

// autoReboot is the --yolo last-resort path when modprobe + PCIe rescan
// have failed to bring the host's mlx5_core PF back. Issues `sudo
// reboot`, waits for SSH to come back (up to 5 min), and confirms the
// parent_iface is live before returning a fresh client. The caller
// adopts the returned client; the old one is unusable after reboot.
func autoReboot(ctx context.Context, c *ssh.Client, cfg ssh.Config, parentIface, tag string, out io.Writer) (*ssh.Client, error) {
	fmt.Fprintf(out, "%s   still ghost after PCIe rescan; --yolo set, issuing 'sudo reboot' ...\n", tag)
	// `sudo reboot` returns SIGHUP/255 to the SSH client when systemd
	// kills sshd — that's expected, not a failure. Don't surface the
	// exit code to the user; the only thing that matters is that the
	// host actually comes back.
	_ = c.Run(ctx, "sudo -n reboot")
	c.Close()

	fmt.Fprintf(out, "%s   host going down — waiting for SSH on %s to return (up to 5 min) ...\n", tag, cfg.Address)
	deadline := time.Now().Add(5 * time.Minute)
	// Give the box a moment to actually go down before polling — if we
	// dial too fast we'll race the still-alive sshd and false-positive.
	time.Sleep(20 * time.Second)
	var newClient *ssh.Client
	for time.Now().Before(deadline) {
		dialCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		nc, err := ssh.Dial(dialCtx, cfg)
		cancel()
		if err == nil {
			newClient = nc
			break
		}
		time.Sleep(5 * time.Second)
	}
	if newClient == nil {
		return nil, fmt.Errorf("host did not return on SSH within 5 min after reboot — investigate manually (the box may be hung in BIOS/POST or VFIO reset)")
	}
	fmt.Fprintf(out, "%s   SSH back; re-checking %s ...\n", tag, parentIface)

	// Re-check parent_iface after the reboot. If it's still ghost, no
	// amount of further automation will help — surface the failure.
	r := newClient.Run(ctx, fmt.Sprintf("ethtool -i %s 2>&1", parentIface))
	combined := strings.TrimSpace(r.Stdout + r.Stderr)
	if r.OK() && strings.Contains(combined, "mlx5_core") {
		fmt.Fprintf(out, "%s   %s live after reboot.\n", tag, parentIface)
		return newClient, nil
	}
	newClient.Close()
	return nil, fmt.Errorf("parent iface %s still in ghost state after reboot — manual diagnosis required (lspci -d 15b3: on the host, check dmesg for mlx5 probe errors)", parentIface)
}

func renderHostNetplan(parent string, vlans []poc.HostDataPlaneVLAN, defaultMTU int) string {
	var b strings.Builder
	b.WriteString("# Managed by dpubnkctl — DO NOT EDIT.\n")
	b.WriteString("# Data-plane VLAN sub-interfaces for k8s east-west + control-plane traffic.\n")
	b.WriteString("network:\n")
	b.WriteString("  version: 2\n")
	b.WriteString("  renderer: networkd\n")
	b.WriteString("  vlans:\n")
	for _, v := range vlans {
		mtu := v.MTU
		if mtu == 0 {
			mtu = defaultMTU
		}
		fmt.Fprintf(&b, "    %s:\n", v.PortName())
		fmt.Fprintf(&b, "      id: %d\n", v.Tag)
		fmt.Fprintf(&b, "      link: %s\n", parent)
		fmt.Fprintf(&b, "      mtu: %d\n", mtu)
		fmt.Fprintf(&b, "      addresses:\n        - %s\n", v.IP)
	}
	return b.String()
}

func stripCIDR(s string) string {
	if i := strings.IndexByte(s, '/'); i > 0 {
		return s[:i]
	}
	return s
}

func indent(s, prefix string) string {
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		if line == "" {
			b.WriteString("\n")
			continue
		}
		b.WriteString(prefix)
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

