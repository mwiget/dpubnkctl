package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/mwiget/dpubnkctl/internal/discover"
	"github.com/mwiget/dpubnkctl/internal/poc"
	"github.com/mwiget/dpubnkctl/internal/ssh"
)

type discoverRangeFlags struct {
	pocDir       string
	sshUser      string
	sshKey       string
	sshPort      int
	jumphost     string
	jumpUser     string
	jumpKey      string
	concurrency  int
	dialTimeout  time.Duration
	probeTimeout time.Duration
	noUpdate     bool
}

func newDiscoverRangeCmd() *cobra.Command {
	f := &discoverRangeFlags{}
	cmd := &cobra.Command{
		Use:   "range <ip-range>",
		Short: "Scan a range of IPs over SSH, classify reachable hosts",
		Long: `Probe every IP in the range concurrently and run the full host probe
battery on every reachable SSH endpoint.

Range syntax:
  192.168.68.66                  single IP
  192.168.68.66-71               last-octet shorthand
  192.168.68.66-192.168.68.71    full range
  192.168.68.0/24                CIDR (network + broadcast stripped for /<=30)

Per-IP results stream live. Reachable hosts are merged into poc.yaml and
inventory/<hostname>/discover.json. Unreachable IPs are listed at the end
with a short reason.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDiscoverRange(cmd.Context(), cmd.OutOrStdout(), args[0], f)
		},
	}
	cmd.Flags().StringVar(&f.pocDir, "poc", "", "PoC repo path (default: current directory)")
	cmd.Flags().StringVar(&f.sshUser, "ssh-user", "root", "SSH user (shared across the range)")
	cmd.Flags().StringVar(&f.sshKey, "ssh-key", "", "Path to SSH private key (required, shared)")
	cmd.Flags().IntVar(&f.sshPort, "ssh-port", 22, "SSH port")
	cmd.Flags().StringVar(&f.jumphost, "jumphost", "", "Jumphost address (optional, format host[:port])")
	cmd.Flags().StringVar(&f.jumpUser, "jumphost-user", "", "Jumphost SSH user (defaults to --ssh-user)")
	cmd.Flags().StringVar(&f.jumpKey, "jumphost-key", "", "Jumphost SSH private key (defaults to --ssh-key)")
	cmd.Flags().IntVar(&f.concurrency, "concurrency", 8, "Max in-flight SSH probes")
	cmd.Flags().DurationVar(&f.dialTimeout, "dial-timeout", 4*time.Second, "Per-IP SSH dial timeout (unreachable IPs)")
	cmd.Flags().DurationVar(&f.probeTimeout, "probe-timeout", 60*time.Second, "Per-host probe budget once SSH connects")
	cmd.Flags().BoolVar(&f.noUpdate, "no-update", false, "Print findings only; do not modify poc.yaml or inventory/")
	_ = cmd.MarkFlagRequired("ssh-key")
	return cmd
}

func runDiscoverRange(ctx context.Context, out io.Writer, rangeArg string, f *discoverRangeFlags) error {
	repo, err := resolvePoCDir(f.pocDir)
	if err != nil {
		return err
	}
	p, err := poc.Load(repo)
	if err != nil {
		return fmt.Errorf("not a PoC repo (%s): %w", repo, err)
	}

	ips, err := discover.ParseRange(rangeArg)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Scanning %d IPs (%s) with concurrency=%d ...\n\n", len(ips), rangeArg, f.concurrency)

	known := filepath.Join(repo, "inventory", "known_hosts")
	base := ssh.Config{
		Port:       f.sshPort,
		User:       f.sshUser,
		KeyPath:    f.sshKey,
		KnownHosts: known,
	}
	if f.jumphost != "" {
		jumpUser := f.jumpUser
		if jumpUser == "" {
			jumpUser = f.sshUser
		}
		jumpKey := f.jumpKey
		if jumpKey == "" {
			jumpKey = f.sshKey
		}
		base.Jumphost = &ssh.Config{
			Address: f.jumphost, Port: 22, User: jumpUser, KeyPath: jumpKey,
			KnownHosts: known, Timeout: f.dialTimeout,
		}
	}

	results := discover.ScanRange(ctx, ips, discover.ScanOptions{
		BaseSSH:      base,
		DialTimeout:  f.dialTimeout,
		ProbeTimeout: f.probeTimeout,
		Concurrency:  f.concurrency,
	})

	var (
		reachable   []discover.ScanItem
		unreachable []discover.ScanItem
	)
	for item := range results {
		if !item.Reachable {
			fmt.Fprintf(out, "  [skip] %-15s  %s\n", item.IP.String(), item.Reason)
			unreachable = append(unreachable, item)
			continue
		}
		if item.Err != nil {
			fmt.Fprintf(out, "  [err]  %-15s  %v\n", item.IP.String(), item.Err)
			continue
		}
		r := item.Result
		hostname := r.Host.Hostname
		if hostname == "" {
			hostname = item.IP.String()
		}
		fmt.Fprintf(out, "  [ok]   %-15s  %s — %s, %d DPU(s)\n",
			item.IP.String(), hostname, orDash(r.Host.OS.PrettyName), len(r.DPUs))
		reachable = append(reachable, item)
	}

	// Sort for deterministic output.
	sort.Slice(reachable, func(i, j int) bool {
		return ipLess(reachable[i].IP, reachable[j].IP)
	})

	fmt.Fprintf(out, "\nSummary: %d reachable, %d unreachable\n", len(reachable), len(unreachable))

	if f.noUpdate {
		fmt.Fprintln(out, "\n--no-update: poc.yaml and inventory/ left unchanged.")
		return nil
	}

	hostFlags := &discoverHostFlags{
		sshUser: f.sshUser,
		sshKey:  f.sshKey,
		sshPort: f.sshPort,
		jumphost: f.jumphost,
	}
	for _, item := range reachable {
		r := item.Result
		hostname := r.Host.Hostname
		if hostname == "" {
			hostname = sanitizeHostKey(item.IP.String())
		}
		invDir := filepath.Join(repo, "inventory", hostname)
		if err := os.MkdirAll(invDir, 0o755); err != nil {
			return err
		}
		if err := writeJSON(filepath.Join(invDir, "discover.json"), r); err != nil {
			return err
		}
		updatePoCWithHost(p, hostname, item.IP.String(), hostFlags, f.sshKey, r)
		if err := appendDiscoverJournal(repo, hostname, item.IP.String(), r); err != nil {
			return err
		}
	}
	if err := p.Save(repo); err != nil {
		return err
	}

	fmt.Fprintf(out, "Updated poc.yaml: %d host(s) merged\n", len(reachable))
	return nil
}

// ipLess compares two IPv4 addresses. Both inputs may be in 4- or
// 16-byte form (net.IPv4 vs raw 4-byte slices); we normalize to 4 bytes.
func ipLess(a, b net.IP) bool {
	return bytes.Compare(a.To4(), b.To4()) < 0
}
