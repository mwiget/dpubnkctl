package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/mwiget/dpubnkctl/internal/poc"
	"github.com/mwiget/dpubnkctl/internal/provision"
	"github.com/mwiget/dpubnkctl/internal/ssh"
)

func newProvisionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "provision",
		Short: "Flash and configure DPUs",
	}
	cmd.AddCommand(newProvisionPlanCmd())
	cmd.AddCommand(&cobra.Command{
		Use:   "dpu <hostname>",
		Short: "Execute the flash plan against a host (destructive — gated by --yolo)",
		Args:  cobra.ExactArgs(1),
		RunE:  notYet("provision dpu", "the next iteration of Phase 2"),
	})
	return cmd
}

type provisionPlanFlags struct {
	pocDir  string
	dpuPCI  string // pick which DPU on the host (default: first)
	write   string // optional path to dump rendered bf.conf
	timeout time.Duration
}

func newProvisionPlanCmd() *cobra.Command {
	f := &provisionPlanFlags{}
	cmd := &cobra.Command{
		Use:   "plan <hostname>",
		Short: "Render bf.conf + run pre-flash readiness checks (no destructive ops)",
		Long: `Build the bf.conf that would be sent to <hostname>'s DPU and run the
pre-flash readiness probes over SSH. Nothing is written to the host.

Use this before 'dpubnkctl provision dpu' to verify the plan with the
pre-sales SE persona and the customer.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runProvisionPlan(cmd.Context(), cmd.OutOrStdout(), args[0], f)
		},
	}
	cmd.Flags().StringVar(&f.pocDir, "poc", "", "PoC repo path (default: current directory)")
	cmd.Flags().StringVar(&f.dpuPCI, "dpu", "", "DPU PCI address (default: first DPU on the host)")
	cmd.Flags().StringVar(&f.write, "write", "", "Write rendered bf.conf to this path (e.g. artifacts/<host>-bf.conf) instead of printing it")
	cmd.Flags().DurationVar(&f.timeout, "timeout", 30*time.Second, "SSH timeout for readiness probes")
	return cmd
}

func runProvisionPlan(ctx context.Context, out io.Writer, hostname string, f *provisionPlanFlags) error {
	repo, err := resolvePoCDir(f.pocDir)
	if err != nil {
		return err
	}
	p, err := poc.Load(repo)
	if err != nil {
		return fmt.Errorf("not a PoC repo (%s): %w", repo, err)
	}

	host, err := findHost(p, hostname)
	if err != nil {
		return err
	}
	dpu, err := pickDPU(host, f.dpuPCI)
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "PoC:    %s   (BNK %s, DOCA %s)\n", p.Metadata.Name, p.Metadata.BNKVersion, p.Versions.DOCA)
	fmt.Fprintf(out, "Host:   %s   (%s@%s)\n", host.Name, host.SSH.User, host.SSH.Address)
	fmt.Fprintf(out, "DPU:    %s   mode=%s lag=%v\n\n", dpu.PCI, orDash(dpu.Mode), dpu.LAG)

	// 1. Render bf.conf.
	fmt.Fprintln(out, "=== bf.conf render ===")
	rendered, rerr := provision.Render(p, host, dpu, repo)
	if rerr != nil {
		fmt.Fprintln(out, "  FAIL:", rerr)
	} else {
		fmt.Fprintf(out, "  ok — %d bytes (%s template)\n", len(rendered), lagTag(dpu.LAG))
		if f.write != "" {
			path := f.write
			if !filepath.IsAbs(path) {
				path = filepath.Join(repo, path)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(path, []byte(rendered), 0o644); err != nil {
				return err
			}
			fmt.Fprintf(out, "  wrote %s\n", path)
		} else {
			fmt.Fprintln(out, "\n--- begin bf.conf ---")
			fmt.Fprintln(out, rendered)
			fmt.Fprintln(out, "--- end bf.conf ---")
		}
	}

	// 2. Readiness checks via SSH.
	fmt.Fprintln(out, "\n=== readiness ===")
	known := filepath.Join(repo, "inventory", "known_hosts")
	keyPath := host.SSH.KeyRef
	if !filepath.IsAbs(keyPath) {
		keyPath = filepath.Join(repo, keyPath)
	}
	cfg := ssh.Config{
		Address: host.SSH.Address, Port: host.SSH.Port,
		User: host.SSH.User, KeyPath: keyPath,
		KnownHosts: known, Timeout: f.timeout,
	}
	if host.SSH.Jumphost != "" {
		cfg.Jumphost = &ssh.Config{
			Address: host.SSH.Jumphost, Port: 22,
			User: host.SSH.User, KeyPath: keyPath,
			KnownHosts: known, Timeout: f.timeout,
		}
	}

	dialCtx, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()
	client, err := ssh.Dial(dialCtx, cfg)
	if err != nil {
		fmt.Fprintf(out, "  ssh dial failed: %v\n", err)
		fmt.Fprintln(out, "\nplan: NOT READY")
		return nil
	}
	defer client.Close()

	probeCtx, pcancel := context.WithTimeout(ctx, 60*time.Second)
	defer pcancel()
	rep := provision.Check(probeCtx, host.Name, provision.AsRunner(client), dpu.PCI)
	for _, c := range rep.Checks {
		marker := "✓"
		switch c.Result {
		case "fail":
			marker = "✗"
		case "warn":
			marker = "!"
		}
		fmt.Fprintf(out, "  %s %-22s  %s\n", marker, c.Name, c.Detail)
	}
	for _, w := range rep.Warnings {
		fmt.Fprintf(out, "  warn: %s\n", w)
	}
	for _, e := range rep.Errors {
		fmt.Fprintf(out, "  err:  %s\n", e)
	}

	switch {
	case rerr != nil:
		fmt.Fprintln(out, "\nplan: NOT READY (bf.conf render failed — fix poc.yaml)")
	case !rep.Ready():
		fmt.Fprintln(out, "\nplan: NOT READY (resolve errors above)")
	default:
		fmt.Fprintln(out, "\nplan: READY — when authorized, run `dpubnkctl provision dpu "+host.Name+"`")
	}
	return nil
}

func findHost(p *poc.PoC, name string) (*poc.Host, error) {
	for i := range p.Hosts {
		if p.Hosts[i].Name == name {
			return &p.Hosts[i], nil
		}
	}
	return nil, fmt.Errorf("host %q not in poc.yaml — run `dpubnkctl discover` first", name)
}

func pickDPU(h *poc.Host, pci string) (*poc.DPU, error) {
	if len(h.DPUs) == 0 {
		return nil, fmt.Errorf("host %q has no DPUs in poc.yaml", h.Name)
	}
	if pci == "" {
		return &h.DPUs[0], nil
	}
	for i := range h.DPUs {
		if h.DPUs[i].PCI == pci {
			return &h.DPUs[i], nil
		}
	}
	return nil, fmt.Errorf("DPU %q not on host %q (have: %v)", pci, h.Name, dpuPCIs(h))
}

func dpuPCIs(h *poc.Host) []string {
	out := make([]string, len(h.DPUs))
	for i, d := range h.DPUs {
		out[i] = d.PCI
	}
	return out
}

func lagTag(lag bool) string {
	if lag {
		return "LAG"
	}
	return "non-LAG"
}
