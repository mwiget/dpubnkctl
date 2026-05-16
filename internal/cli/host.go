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
		if !f.yolo {
			return errors.New("refusing to rewrite netplan without --yolo (or use --dry-run)")
		}
		if f.confirmCluster != p.Metadata.Name {
			return fmt.Errorf("--confirm-cluster must equal poc.yaml.metadata.name (%q), got %q", p.Metadata.Name, f.confirmCluster)
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
			err := setupOneHost(ctx, out, repo, h, p.Network.DPUMTU, f.dryRun, tag)
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

func setupOneHost(ctx context.Context, out io.Writer, repo string, h *poc.Host, defaultMTU int, dryRun bool, tag string) error {
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
	defer c.Close()

	// Pre-flight: catch the post-BFB "ghost PF" state before netplan
	// blunders into opaque RTNETLINK errors. A successful BFB flash
	// leaves the host's mlx5_core PF detached from the kernel — the
	// interface name exists, but `ethtool -i` returns "No such device".
	//
	// Recovery has three tiers (cheapest first):
	//   1. provision dpu's settle wait usually catches this — the host
	//      mlx5_core recovers on its own within 10-30s post-flash. If
	//      it has, the ethtool check below returns clean and we proceed.
	//   2. If still ghost: `modprobe -r mlx5_core; modprobe mlx5_core`.
	//      Re-runs the driver's PCIe probe path; re-creates the netdev.
	//      Seconds, no reboot, no VFIO touch.
	//   3. If still ghost after reload: fail loud with the historical
	//      "reboot the host" message. The operator handles it manually.
	//
	// We avoid host reboot specifically because on Proxmox VFIO-pass-
	// through setups (BlueField-3 attached at PCIe 82:00/c1:00 etc.),
	// a guest reboot during the post-flash window can hang the host
	// kernel's PCIe reset path. Verified on rome1, May 15. See
	// AGENTS.md #11.
	ethtoolCheck := func() (bool, string, error) {
		r := c.Run(ctx, fmt.Sprintf("ethtool -i %s 2>&1", dp.ParentIface))
		combined := strings.TrimSpace(r.Stdout + r.Stderr)
		if r.OK() {
			return true, combined, nil
		}
		return false, combined, nil
	}

	ok, combined, err := ethtoolCheck()
	if err != nil {
		return err
	}
	// Two-tier recovery if ghost detected:
	//   1. `modprobe -r mlx5_core; modprobe mlx5_core` + 90s poll. Drives
	//      a clean kernel re-probe of every mlx5 PCIe device. udev rename
	//      adds another ~5-30s after the kernel probe completes — that's
	//      why we poll 90s, not 30s. (The 30s ceiling in the previous fix
	//      hit the deadline 4s before udev renamed on the May 16 run.)
	//   2. If modprobe reload doesn't recover, fall through to a PCIe-
	//      level remove+rescan via /sys/bus/pci. More targeted than
	//      modprobe (per-device), and works when the driver is stuck on
	//      one specific BDF.
	pollUntilLive := func(deadline time.Time) bool {
		iter := 0
		for {
			time.Sleep(2 * time.Second)
			iter++
			ok, combined, _ = ethtoolCheck()
			if ok && strings.Contains(combined, "mlx5_core") {
				fmt.Fprintf(out, "%s   %s live after %d polls.\n", tag, dp.ParentIface, iter)
				return true
			}
			if time.Now().After(deadline) {
				return false
			}
		}
	}

	if !ok && strings.Contains(combined, "No such device") {
		fmt.Fprintf(out, "%s ghost mlx5_core PF detected on %s; attempting modprobe reload ...\n",
			tag, dp.ParentIface)
		if r := c.Run(ctx, "sudo -n bash -c 'modprobe -r mlx5_core 2>&1 || true; modprobe mlx5_core 2>&1'"); !r.OK() {
			fmt.Fprintf(out, "%s   modprobe reload exit=%d: %s\n",
				tag, r.ExitCode, strings.TrimSpace(r.Stdout+r.Stderr))
		}
		fmt.Fprintf(out, "%s   polling ethtool -i %s for up to 90s ...\n", tag, dp.ParentIface)
		if pollUntilLive(time.Now().Add(90 * time.Second)) {
			fmt.Fprintf(out, "%s   mlx5_core %s recovered after modprobe reload.\n", tag, dp.ParentIface)
		} else {
			fmt.Fprintf(out, "%s   still ghost after modprobe + 90s; trying PCIe remove+rescan ...\n", tag)
			// Remove every BlueField (vendor 15b3) PCIe function, then
			// rescan the bus. Forces a fresh enumeration + driver bind.
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
			if pollUntilLive(time.Now().Add(60 * time.Second)) {
				fmt.Fprintf(out, "%s   mlx5_core %s recovered after PCIe rescan.\n", tag, dp.ParentIface)
			}
		}
	}
	if !ok {
		if strings.Contains(combined, "No such device") || strings.TrimSpace(combined) == "" {
			return fmt.Errorf("parent iface %s did not recover after modprobe reload + PCIe rescan — reboot the host manually (BF3 EMBEDDED_CPU rules out mlxfwreset). See AGENTS.md #11", dp.ParentIface)
		}
		return fmt.Errorf("ethtool -i %s failed: %s", dp.ParentIface, combined)
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

