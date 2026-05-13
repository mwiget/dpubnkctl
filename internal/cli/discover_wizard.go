package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mwiget/dpubnkctl/internal/discover"
	"github.com/mwiget/dpubnkctl/internal/poc"
	"github.com/mwiget/dpubnkctl/internal/ssh"
)

// newDiscoverWizardCmd is the no-LLM-required guided onboarding for
// the discover phase. Walks the operator through:
//
//	1. subnet/range
//	2. SSH user + key (validates key exists)
//	3. optional jumphost
//	4. probe scan (parallel, same engine as `discover range`)
//	5. for each reachable host: prompt for role (control-plane | worker | both)
//	6. write everything to poc.yaml + per-host inventory/<host>/discover.json
//
// The agentic workflow (`dpubnkctl agent claude` etc.) gives you the
// same shape conversationally with much better context awareness — the
// wizard exists for operators without LLM access (air-gapped labs,
// first-time use without API keys).
func newDiscoverWizardCmd() *cobra.Command {
	var pocDir string
	cmd := &cobra.Command{
		Use:   "wizard",
		Short: "Interactive discovery — prompts for subnet, SSH creds, role per host",
		Long: `Walk through host discovery one prompt at a time. Useful for
operators without access to an agentic CLI (which would handle the same
flow conversationally via AGENTS.md).

All inputs map 1:1 to ` + "`dpubnkctl discover range`" + ` flags — the wizard
just gathers them interactively, scans, then prompts for role assignment
per reachable host before merging into poc.yaml.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDiscoverWizard(cmd.Context(), cmd.OutOrStdout(), os.Stdin, pocDir)
		},
	}
	cmd.Flags().StringVar(&pocDir, "poc", "", "PoC repo path (default: current directory)")
	return cmd
}

func runDiscoverWizard(ctx context.Context, out io.Writer, in io.Reader, pocDir string) error {
	repo, err := resolvePoCDir(pocDir)
	if err != nil {
		return err
	}
	p, err := poc.Load(repo)
	if err != nil {
		return fmt.Errorf("not a PoC repo (%s): %w — run `dpubnkctl init` first", repo, err)
	}
	r := bufio.NewReader(in)

	fmt.Fprintf(out, "Discovery wizard for PoC %q (%s)\n\n", p.Metadata.Name, repo)

	// 1. Subnet/range.
	rangeArg := ask(out, r, "Subnet or range to scan",
		"e.g. 192.168.68.0/24 or 192.168.68.66-71 or a single IP",
		"")
	if rangeArg == "" {
		return fmt.Errorf("subnet/range is required")
	}
	ips, err := discover.ParseRange(rangeArg)
	if err != nil {
		return fmt.Errorf("parse range %q: %w", rangeArg, err)
	}
	fmt.Fprintf(out, "→ %d IP(s) to probe\n\n", len(ips))

	// 2. SSH user.
	sshUser := ask(out, r, "SSH user", "shared across the range", "ubuntu")

	// 3. SSH key path. Validate it exists.
	defaultKey := os.ExpandEnv("$HOME/.ssh/id_ed25519")
	if _, err := os.Stat(defaultKey); err != nil {
		defaultKey = ""
	}
	var sshKey string
	for {
		sshKey = ask(out, r, "Path to SSH private key", "must be readable", defaultKey)
		if sshKey == "" {
			fmt.Fprintln(out, "  ! key path required")
			continue
		}
		if _, err := os.Stat(sshKey); err != nil {
			fmt.Fprintf(out, "  ! cannot read %s: %v\n", sshKey, err)
			continue
		}
		break
	}

	// 4. Optional jumphost.
	jumphost := ask(out, r, "Jumphost (optional)", "host[:port], blank for none", "")
	jumpUser := sshUser
	jumpKey := sshKey
	if jumphost != "" {
		jumpUser = ask(out, r, "Jumphost SSH user", "blank reuses target user", sshUser)
		jumpKey = ask(out, r, "Jumphost SSH key", "blank reuses target key", sshKey)
	}

	// 5. Confirm before scanning.
	fmt.Fprintln(out, "")
	fmt.Fprintf(out, "About to scan %d IP(s) — ssh %s with key %s%s\n",
		len(ips), sshUser, sshKey,
		ifThen(jumphost != "", " via jumphost "+jumphost))
	if !confirm(out, r, "Proceed?", true) {
		return fmt.Errorf("cancelled by operator")
	}

	// 6. Build SSH base config + scan.
	known := filepath.Join(repo, "inventory", "known_hosts")
	base := ssh.Config{
		Port: 22, User: sshUser, KeyPath: sshKey, KnownHosts: known,
	}
	if jumphost != "" {
		base.Jumphost = &ssh.Config{
			Address: jumphost, Port: 22, User: jumpUser, KeyPath: jumpKey,
			KnownHosts: known, Timeout: 4 * time.Second,
		}
	}
	fmt.Fprintln(out, "\nScanning ...")
	results := discover.ScanRange(ctx, ips, discover.ScanOptions{
		BaseSSH:      base,
		DialTimeout:  4 * time.Second,
		ProbeTimeout: 60 * time.Second,
		Concurrency:  8,
	})

	type reachable struct {
		ip       string
		hostname string
		result   *discover.Result
	}
	var found []reachable
	var skipped int
	for item := range results {
		if !item.Reachable {
			fmt.Fprintf(out, "  [skip] %-15s  %s\n", item.IP.String(), item.Reason)
			skipped++
			continue
		}
		if item.Err != nil {
			fmt.Fprintf(out, "  [err]  %-15s  %v\n", item.IP.String(), item.Err)
			continue
		}
		hostname := item.Result.Host.Hostname
		if hostname == "" {
			hostname = sanitizeHostKey(item.IP.String())
		}
		fmt.Fprintf(out, "  [ok]   %-15s  %s — %s, %d DPU(s)\n",
			item.IP.String(), hostname, orDash(item.Result.Host.OS.PrettyName), len(item.Result.DPUs))
		found = append(found, reachable{ip: item.IP.String(), hostname: hostname, result: item.Result})
	}
	sort.Slice(found, func(i, j int) bool { return found[i].ip < found[j].ip })
	fmt.Fprintf(out, "\nReachable: %d   Skipped: %d\n\n", len(found), skipped)

	if len(found) == 0 {
		return fmt.Errorf("no reachable hosts — nothing to merge")
	}

	// 7. Per-host role assignment (the SE call).
	fmt.Fprintln(out, "Now assign each reachable host a cluster role.")
	fmt.Fprintln(out, "  control-plane = k8s control plane only")
	fmt.Fprintln(out, "  worker        = k8s worker only")
	fmt.Fprintln(out, "  both          = control plane AND worker (typical 2-node PoC)")
	fmt.Fprintln(out, "")
	for i := range found {
		def := suggestRole(len(found))
		choice := askChoice(out, r,
			fmt.Sprintf("Role for %s (%s, %d DPU(s))",
				found[i].hostname, found[i].ip, len(found[i].result.DPUs)),
			[]string{"both", "control-plane", "worker", "skip"}, def)
		if choice == "skip" {
			fmt.Fprintf(out, "  → skipping %s (won't appear in poc.yaml)\n", found[i].hostname)
			found[i].hostname = "" // marker
			continue
		}
		// Persist inventory + merge into poc.yaml.
		invDir := filepath.Join(repo, "inventory", found[i].hostname)
		if err := os.MkdirAll(invDir, 0o755); err != nil {
			return err
		}
		if err := writeJSON(filepath.Join(invDir, "discover.json"), found[i].result); err != nil {
			return err
		}
		hostFlags := &discoverHostFlags{
			sshUser: sshUser, sshKey: sshKey, sshPort: 22, jumphost: jumphost,
			role: choice,
		}
		updatePoCWithHost(p, found[i].hostname, found[i].ip, hostFlags, sshKey, found[i].result)
		_ = appendDiscoverJournal(repo, found[i].hostname, found[i].ip, found[i].result)
	}

	if err := p.Save(repo); err != nil {
		return err
	}
	merged := 0
	for _, h := range found {
		if h.hostname != "" {
			merged++
		}
	}
	fmt.Fprintf(out, "\nDONE.  %d host(s) merged into poc.yaml.\n", merged)
	fmt.Fprintln(out, "Next: edit poc.yaml to set per-DPU VLANs + IPs, then `dpubnkctl provision dpus`.")
	return nil
}

// suggestRole picks the default role offered for the per-host prompt.
// 2 reachable hosts → both is the most common BNK PoC topology.
// 3+ → control-plane (operator usually wants 3 CPs + workers).
// 1 → both (single-node lab).
func suggestRole(n int) string {
	if n >= 3 {
		return "control-plane"
	}
	return "both"
}

// ask prints "label [hint] (default): " and reads a line. Empty input
// returns def. Trims whitespace.
func ask(out io.Writer, r *bufio.Reader, label, hint, def string) string {
	tail := ""
	if hint != "" {
		tail += " — " + hint
	}
	if def != "" {
		tail += fmt.Sprintf(" [%s]", def)
	}
	fmt.Fprintf(out, "%s%s: ", label, tail)
	line, err := r.ReadString('\n')
	if err != nil && line == "" {
		return def
	}
	v := strings.TrimSpace(line)
	if v == "" {
		return def
	}
	return v
}

// askChoice prompts with a fixed set of options; default in brackets.
// Re-prompts on invalid input.
func askChoice(out io.Writer, r *bufio.Reader, label string, choices []string, def string) string {
	for {
		fmt.Fprintf(out, "%s [%s] (%s): ", label, def, strings.Join(choices, "|"))
		line, _ := r.ReadString('\n')
		v := strings.TrimSpace(line)
		if v == "" {
			return def
		}
		for _, c := range choices {
			if strings.EqualFold(v, c) {
				return c
			}
		}
		fmt.Fprintf(out, "  ! pick one of: %s\n", strings.Join(choices, ", "))
	}
}

// confirm asks a y/N (or y/Y default) and returns true on yes.
func confirm(out io.Writer, r *bufio.Reader, label string, def bool) bool {
	yn := "y/N"
	if def {
		yn = "Y/n"
	}
	fmt.Fprintf(out, "%s [%s]: ", label, yn)
	line, _ := r.ReadString('\n')
	v := strings.ToLower(strings.TrimSpace(line))
	if v == "" {
		return def
	}
	return v == "y" || v == "yes"
}

func ifThen(cond bool, s string) string {
	if cond {
		return s
	}
	return ""
}
