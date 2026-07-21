package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mwiget/dpubnkctl/internal/poc"
	"github.com/mwiget/dpubnkctl/internal/provision"
	"github.com/mwiget/dpubnkctl/internal/ssh"
)

type provisionSetModeFlags struct {
	pocDir         string
	dpuPCI         string
	mode           string
	yolo           bool
	confirmCluster string
	stageOnly      bool
	timeout        time.Duration
}

func newProvisionSetModeCmd() *cobra.Command {
	f := &provisionSetModeFlags{}
	cmd := &cobra.Command{
		Use:   "set-mode <hostname>",
		Short: "Switch a DPU between DPU mode (EMBEDDED_CPU) and NIC mode (SEPARATED_HOST)",
		Long: `Set the BlueField personality knob INTERNAL_CPU_MODEL on one host's
DPU(s) and apply it with a firmware reset:

  --mode dpu   EMBEDDED_CPU(1) — the Arm cores own the NIC (a DPU / k8s node)
  --mode nic   SEPARATED_HOST(0) — the host owns the NIC (plain ConnectX)

mlxconfig stages the change (Next Boot), then mlxfwreset applies it to the
live value (Current). A plain host reboot is NOT enough — the BlueField
keeps its running config across a warm reboot, so a firmware reset (default
level 3: driver restart + PCI reset) is required to flip the live mode. By
default this command runs mlxfwreset for you and verifies the new Current
value.

The reset briefly disrupts the DPU (and the host PF), so applying it is
gated like every destructive command:
  --yolo                   acknowledge the mlxfwreset
  --confirm-cluster NAME   must equal poc.yaml.metadata.name (typo guard)

Use --stage-only to write mlxconfig without resetting (you apply it later
with mlxfwreset or a cold power cycle); staging only writes Next Boot and is
not gated.

Only INTERNAL_CPU_MODEL is touched — never the ownership sub-knobs. Both
mlxconfig and mlxfwreset run host-side against the DPU's PCI address. Where
host management rides the DPU being reset, mlxfwreset may drop connectivity
and a BMC cold power cycle is the alternative.

Examples:
  dpubnkctl provision set-mode dpu-server-2 --mode nic --yolo --confirm-cluster my-poc
  dpubnkctl provision set-mode dpu-server-2 --mode dpu --dpu 0000:03:00.0 --stage-only`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runProvisionSetMode(cmd.Context(), cmd.OutOrStdout(), args[0], f)
		},
	}
	cmd.Flags().StringVar(&f.pocDir, "poc", "", "PoC repo path (default: current directory)")
	cmd.Flags().StringVar(&f.dpuPCI, "dpu", "", "DPU PCI address (default: every DPU on the host)")
	cmd.Flags().StringVar(&f.mode, "mode", "", "Target mode: nic | dpu (required)")
	cmd.Flags().BoolVar(&f.yolo, "yolo", false, "Acknowledge the mlxfwreset (firmware reset) needed to apply the mode change")
	cmd.Flags().StringVar(&f.confirmCluster, "confirm-cluster", "", "Must equal poc.yaml.metadata.name (typo guard); required to apply (not with --stage-only)")
	cmd.Flags().BoolVar(&f.stageOnly, "stage-only", false, "Only stage the mlxconfig write; don't run mlxfwreset (you apply it later)")
	cmd.Flags().DurationVar(&f.timeout, "timeout", 30*time.Second, "SSH dial/probe timeout")
	return cmd
}

func runProvisionSetMode(ctx context.Context, out io.Writer, hostname string, f *provisionSetModeFlags) error {
	if f.mode == "" {
		return fmt.Errorf("--mode is required (nic | dpu)")
	}
	mode, err := provision.ParseMode(f.mode)
	if err != nil {
		return err
	}
	repo, err := resolvePoCDir(f.pocDir)
	if err != nil {
		return err
	}
	p, err := poc.Load(repo)
	if err != nil {
		return fmt.Errorf("not a PoC repo (%s): %w", repo, err)
	}
	// The mlxfwreset that applies the change is destructive, so require both
	// gates whenever we're actually applying. --stage-only only writes Next
	// Boot (nothing flips live) and is not gated.
	if !f.stageOnly {
		if err := requireTwoGates(f.yolo, "--confirm-cluster", f.confirmCluster, p.Metadata.Name, "DPU mode change"); err != nil {
			return err
		}
	}
	host, err := findHost(p, hostname)
	if err != nil {
		return err
	}
	dpus, err := selectDPUs(host, f.dpuPCI)
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "Host:   %s   (%s@%s)\n", host.Name, host.SSH.User, host.SSH.Address)
	fmt.Fprintf(out, "Target: %s on %d DPU(s)\n\n", mode.Describe(), len(dpus))

	opts := setModeOptions{apply: !f.stageOnly, yolo: f.yolo, timeout: f.timeout}
	if err := applyCPUModeToHost(ctx, out, repo, host, dpus, mode, opts); err != nil {
		return err
	}

	// Persist the observed mode only when we actually applied + verified it.
	// --stage-only leaves poc.yaml alone since the card hasn't switched yet.
	if opts.apply && opts.yolo {
		for _, d := range dpus {
			d.Mode = mode.String()
		}
		if err := savePoC(repo, p, out); err != nil {
			return err
		}
	}
	fmt.Fprintln(out, "\nset-mode: done")
	return nil
}

// selectDPUs returns the target DPU pointers on host: just the one matching
// pci, or every DPU when pci is empty. Pointers index into host.DPUs so a
// caller can mutate .Mode and persist it.
func selectDPUs(h *poc.Host, pci string) ([]*poc.DPU, error) {
	if len(h.DPUs) == 0 {
		return nil, fmt.Errorf("host %q has no DPUs in poc.yaml", h.Name)
	}
	if pci == "" {
		out := make([]*poc.DPU, len(h.DPUs))
		for i := range h.DPUs {
			out[i] = &h.DPUs[i]
		}
		return out, nil
	}
	for i := range h.DPUs {
		if h.DPUs[i].PCI == pci {
			return []*poc.DPU{&h.DPUs[i]}, nil
		}
	}
	return nil, fmt.Errorf("DPU %q not on host %q (have: %v)", pci, h.Name, dpuPCIs(h))
}

// setModeOptions controls how applyCPUModeToHost applies a mode change.
type setModeOptions struct {
	apply   bool          // false (--stage-only) = write mlxconfig but don't mlxfwreset
	yolo    bool          // required to run the disruptive firmware reset
	timeout time.Duration // SSH dial/probe timeout
}

// applyCPUModeToHost sets INTERNAL_CPU_MODEL on the given DPUs of one host,
// then (unless staging only) applies each change with an mlxfwreset firmware
// reset and verifies each DPU's live Current value reports the target mode.
// DPUs already in the target mode are skipped. It does not persist poc.yaml
// — the caller decides that.
//
// A plain host reboot does NOT apply the change (the DPU keeps its running
// config across a warm reboot — verified on BF3), so the apply is a firmware
// reset, not rebootHostAndWait. Shared by `provision set-mode` and teardown's
// --nic-mode.
func applyCPUModeToHost(ctx context.Context, out io.Writer, repo string, host *poc.Host, dpus []*poc.DPU, mode provision.CPUMode, opts setModeOptions) error {
	cfg, err := sshConfigForHost(repo, host, opts.timeout)
	if err != nil {
		return err
	}
	dialCtx, cancel := context.WithTimeout(ctx, opts.timeout)
	client, err := ssh.Dial(dialCtx, cfg)
	cancel()
	if err != nil {
		return fmt.Errorf("ssh dial %s: %w", host.Name, err)
	}
	// client is re-dialed after the reset; the deferred close reads the
	// final value.
	defer func() {
		if client != nil {
			client.Close()
		}
	}()

	runner := provision.AsRunner(client)
	var staged []*poc.DPU
	for _, d := range dpus {
		cur, rerr := provision.ReadCPUMode(ctx, runner, d.PCI)
		switch {
		case rerr != nil:
			fmt.Fprintf(out, "  %s %s: WARN could not read current mode (%v) — setting anyway\n", host.Name, d.PCI, rerr)
		case cur == mode:
			fmt.Fprintf(out, "  %s %s: already %s — no change\n", host.Name, d.PCI, mode)
			continue
		default:
			fmt.Fprintf(out, "  %s %s: %s → %s\n", host.Name, d.PCI, cur, mode)
		}
		if r := client.Run(ctx, provision.SetCPUModeCmd(d.PCI, mode)); !r.OK() {
			return fmt.Errorf("%s %s: mlxconfig set INTERNAL_CPU_MODEL failed: %s", host.Name, d.PCI, strings.TrimSpace(r.Stderr+r.Stdout))
		}
		staged = append(staged, d)
	}

	if len(staged) == 0 {
		fmt.Fprintf(out, "  %s: all target DPUs already in %s mode — nothing to apply\n", host.Name, mode)
		return nil
	}
	if !opts.apply {
		fmt.Fprintf(out, "  %s: %d DPU(s) staged to %s mode — apply with `mlxfwreset -d <pci> -y reset` (or a cold power cycle), then re-run to verify\n", host.Name, len(staged), mode)
		return nil
	}
	if !opts.yolo {
		return fmt.Errorf("%s: %d DPU(s) staged to %s mode but --yolo not set — pass --yolo to run mlxfwreset now, or --stage-only to apply it manually later", host.Name, len(staged), mode)
	}

	// Apply each staged change via a firmware reset. A warm reboot would
	// leave Current unchanged, so mlxfwreset (default level 3) is what flips
	// the live mode.
	for _, d := range staged {
		fmt.Fprintf(out, "  %s %s: applying via mlxfwreset (firmware reset) ...\n", host.Name, d.PCI)
		if err := fwResetWithRetry(ctx, out, client, host.Name, d.PCI); err != nil {
			return err
		}
	}

	// The reset restarts the ARM side and re-enumerates the PF; re-dial a
	// fresh client (the session may blip on setups where host management
	// rides the reset device) before reading back the live mode.
	client.Close()
	client = nil
	if c := redialHost(ctx, cfg, opts.timeout, 90*time.Second); c != nil {
		client = c
	} else {
		return fmt.Errorf("%s: host unreachable after mlxfwreset — if it doesn't return, a cold power cycle / console may be needed", host.Name)
	}

	var bad []string
	for _, d := range staged {
		// mlxfwreset leaves the PF re-enumerating for a while — mlxconfig
		// returns empty until the device is back — so retry the read.
		got, rerr := readModeSettle(ctx, client, d.PCI, 120*time.Second)
		switch {
		case rerr != nil:
			bad = append(bad, fmt.Sprintf("%s verify read failed: %v", d.PCI, rerr))
		case got != mode:
			bad = append(bad, fmt.Sprintf("%s still %s", d.PCI, got))
		default:
			fmt.Fprintf(out, "  %s %s: confirmed %s\n", host.Name, d.PCI, got.Describe())
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("%s: %s mode not applied after mlxfwreset: %s", host.Name, mode, strings.Join(bad, "; "))
	}
	return nil
}

// fwResetWithRetry runs mlxfwreset for one DPU, retrying the transient
// "Synchronization by driver is not supported in the current state" refusal
// that mlxfwreset returns when the device is still settling from a prior
// reset or a just-applied mode switch (seen on back-to-back switches). The
// refusal happens before any reset executes, so the SSH client and device
// are unchanged and safe to retry on. A persistent failure surfaces the
// last message plus the cold-power-cycle hint.
func fwResetWithRetry(ctx context.Context, out io.Writer, client *ssh.Client, hostName, pci string) error {
	const attempts = 4
	const gap = 30 * time.Second
	var lastMsg string
	for i := 1; i <= attempts; i++ {
		r := client.Run(ctx, provision.FWResetCmd(pci))
		if r.OK() {
			return nil
		}
		lastMsg = strings.TrimSpace(r.Stderr + r.Stdout)
		if i < attempts {
			fmt.Fprintf(out, "  %s %s: mlxfwreset not ready yet (%s) — retrying in %s (%d/%d)\n",
				hostName, pci, lastMsg, gap, i, attempts)
			select {
			case <-time.After(gap):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return fmt.Errorf("%s %s: mlxfwreset failed after %d attempts (if host management rides this DPU, a BMC cold power cycle is required instead): %s", hostName, pci, attempts, lastMsg)
}

// readModeSettle reads the DPU's live mode, retrying on read errors — after
// an mlxfwreset the PF briefly disappears from mlxconfig while it
// re-enumerates, so an early read returns empty. Retries until the read
// parses or the deadline passes.
func readModeSettle(ctx context.Context, client *ssh.Client, pci string, within time.Duration) (provision.CPUMode, error) {
	runner := provision.AsRunner(client)
	deadline := time.Now().Add(within)
	var last error
	for {
		got, err := provision.ReadCPUMode(ctx, runner, pci)
		if err == nil {
			return got, nil
		}
		last = err
		if time.Now().After(deadline) || ctx.Err() != nil {
			return provision.ModeUnknown, last
		}
		time.Sleep(5 * time.Second)
	}
}

// redialHost re-establishes an SSH client to the host after a firmware
// reset, retrying until deadline. Returns nil if the host never answers.
func redialHost(ctx context.Context, cfg ssh.Config, dialTimeout, within time.Duration) *ssh.Client {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		dctx, cancel := context.WithTimeout(ctx, dialTimeout)
		c, err := ssh.Dial(dctx, cfg)
		cancel()
		if err == nil {
			return c
		}
		if ctx.Err() != nil {
			return nil
		}
		time.Sleep(5 * time.Second)
	}
	return nil
}

// setAllDPUsMode applies mode to every DPU in the PoC, one host at a time
// (each staged DPU is applied with its own mlxfwreset). Used by teardown's
// --nic-mode. Errors are collected so one unreachable host doesn't abort the
// rest.
func setAllDPUsMode(ctx context.Context, out io.Writer, repo string, p *poc.PoC, mode provision.CPUMode, opts setModeOptions) error {
	var errs []string
	for i := range p.Hosts {
		h := &p.Hosts[i]
		if len(h.DPUs) == 0 {
			continue
		}
		dpus := make([]*poc.DPU, len(h.DPUs))
		for j := range h.DPUs {
			dpus[j] = &h.DPUs[j]
		}
		if err := applyCPUModeToHost(ctx, out, repo, h, dpus, mode, opts); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s-mode revert: %s", mode, strings.Join(errs, "; "))
	}
	return nil
}
