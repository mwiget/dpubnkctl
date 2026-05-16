package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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

	// 2a. SSH port. Hardcoded 22 was hostile to labs running SSH on
	// non-standard ports (jumphost shells, cloud images that disable 22
	// for botnet hygiene). Re-prompt on out-of-range input.
	sshPort := askPort(out, r, "SSH port", "shared across the range", 22)

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
		Port: sshPort, User: sshUser, KeyPath: sshKey, KnownHosts: known,
	}
	if jumphost != "" {
		// Jumphost port stays 22 — that's the most common case and a
		// host:port pair is also accepted in the jumphost address
		// itself, so a non-22 jumphost is still expressible.
		base.Jumphost = &ssh.Config{
			Address: jumphost, Port: 22, User: jumpUser, KeyPath: jumpKey,
			KnownHosts: known, Timeout: 4 * time.Second,
		}
	}
	fmt.Fprintf(out, "\nScanning %d IP(s) ...\n", len(ips))
	scanStart := time.Now()
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
	var skipped, dpuOSes int
	done := 0
	N := len(ips)
	// Heartbeat ticker — fires only if the scan goes silent (no
	// completions) for 10s, so the operator sees something other than
	// `Scanning N IP(s) ...` while every probe sits in a 60s ssh dial.
	// Reset on each completion so a healthy stream of results doesn't
	// trigger it.
	heartbeat := time.NewTicker(10 * time.Second)
	defer heartbeat.Stop()
	resultsLoop:
	for {
		select {
		case item, ok := <-results:
			if !ok {
				break resultsLoop
			}
			done++
			heartbeat.Reset(10 * time.Second)
			prefix := fmt.Sprintf("[%*d/%d]", len(fmt.Sprintf("%d", N)), done, N)
			if !item.Reachable {
				fmt.Fprintf(out, "  %s [skip] %-15s  %s\n", prefix, item.IP.String(), item.Reason)
				skipped++
				continue
			}
			if item.Err != nil {
				fmt.Fprintf(out, "  %s [err]  %-15s  %v\n", prefix, item.IP.String(), item.Err)
				continue
			}
			hostname := item.Result.Host.Hostname
			if hostname == "" {
				hostname = sanitizeHostKey(item.IP.String())
			}
			// A reachable IP that's actually a BlueField DPU OS (PCI bridges
			// in 15b3:*) must NOT enter the host-candidate list. It belongs
			// as a child of its server's dpus[] block, populated later by
			// the per-host discover under the parent server's identity.
			if item.Result.IsDPU {
				fmt.Fprintf(out, "  %s [dpu]  %-15s  %s — DPU OS detected; excluded from host list\n",
					prefix, item.IP.String(), hostname)
				dpuOSes++
				continue
			}
			fmt.Fprintf(out, "  %s [ok]   %-15s  %s — %s, %d DPU(s)\n",
				prefix, item.IP.String(), hostname, orDash(item.Result.Host.OS.PrettyName), len(item.Result.DPUs))
			found = append(found, reachable{ip: item.IP.String(), hostname: hostname, result: item.Result})
		case <-heartbeat.C:
			fmt.Fprintf(out, "  ... %d/%d done, elapsed %s (probes can take up to 60s each)\n",
				done, N, time.Since(scanStart).Round(time.Second))
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].ip < found[j].ip })
	fmt.Fprintf(out, "\nScanned %d IP(s) in %s.\n", N, time.Since(scanStart).Round(time.Second))
	fmt.Fprintf(out, "Reachable hosts: %d   DPU OS detected: %d   Unreachable: %d\n\n",
		len(found), dpuOSes, skipped)

	if len(found) == 0 {
		// Scan completed without error; the range simply had no SSH-
		// reachable hosts. Don't surface this as `error:` exit 1 — that
		// reads as if the tool itself blew up and sends operators on a
		// wild-goose chase. Exit cleanly with diagnostics instead.
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "No reachable hosts in the scanned range. Common causes:")
		fmt.Fprintln(out, "  • Subnet/range is wrong — try a single known IP first.")
		fmt.Fprintln(out, "  • SSH is on a different port than the one you entered.")
		fmt.Fprintln(out, "  • SSH key doesn't authenticate as the user you supplied.")
		fmt.Fprintln(out, "  • mgmt VLAN/firewall is blocking outbound SSH from this host.")
		fmt.Fprintln(out, "")
		fmt.Fprintln(out, "Re-run with the correct range/credentials.")
		return nil
	}

	// 7. Per-host role assignment (the SE call). Pre-compute the count
	// of DPU-free reachable hosts; suggestRole uses it to propose
	// "control-plane" for DPU-free hosts and "worker" for DPU-bearing
	// hosts when enough dedicated CP candidates exist.
	noDPUHosts := 0
	for _, h := range found {
		if len(h.result.DPUs) == 0 {
			noDPUHosts++
		}
	}
	fmt.Fprintln(out, "Now assign each reachable host a cluster role.")
	fmt.Fprintln(out, "A k8s control plane needs 1 node (lab) or 3+ nodes (HA — etcd needs an odd quorum).")
	fmt.Fprintln(out, "The wizard suggests roles based on what's reachable; override per host as needed.")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "  control-plane = k8s control plane only")
	fmt.Fprintln(out, "  worker        = k8s worker only")
	fmt.Fprintln(out, "  both          = control plane AND worker (typical 2-node PoC)")
	fmt.Fprintln(out, "")
	for i := range found {
		dpuCount := len(found[i].result.DPUs)
		def, rationale := suggestRole(dpuCount, len(found), noDPUHosts)
		fmt.Fprintf(out, "  → suggested %s for %s — %s\n", def, found[i].hostname, rationale)
		choice := askChoice(out, r,
			fmt.Sprintf("Role for %s (%s, %d DPU(s))",
				found[i].hostname, found[i].ip, dpuCount),
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
			sshUser: sshUser, sshKey: sshKey, sshPort: sshPort, jumphost: jumphost,
			role: choice,
		}
		mergedInto := updatePoCWithHost(p, found[i].hostname, found[i].ip, hostFlags, sshKey, found[i].result)
		// Auto-fill DPU hostname + tmfifo_ip defaults so the embedded
		// validate at the end of the wizard doesn't immediately yell about
		// two errors per DPU the wizard just merged. Conventions:
		//   - DPU OS hostname: <host>-bf3 (documented in examples/README.md)
		//   - tmfifo_ip:       192.168.100.2/30 (per-host private host↔DPU link)
		// Operator can still override either by editing poc.yaml before
		// running provision.
		fillDPUWizardDefaults(p, found[i].ip, found[i].hostname)
		if mergedInto {
			fmt.Fprintf(out, "  [merged] %s — existing hosts[] entry preserved; only empty fields filled\n", found[i].hostname)
		} else {
			fmt.Fprintf(out, "  [new]    %s — appended to hosts[]\n", found[i].hostname)
		}
		_ = appendDiscoverJournal(repo, found[i].hostname, found[i].ip, found[i].result)
	}

	if err := savePoC(repo, p, out); err != nil {
		return err
	}
	merged := 0
	for _, h := range found {
		if h.hostname != "" {
			merged++
		}
	}
	fmt.Fprintf(out, "\nDONE.  %d host(s) merged into poc.yaml.\n", merged)

	// Wizard handles the SSH/role discovery slice; everything else
	// (LAG vs non-LAG, VLAN tags + subnets, MTU, cluster apiserver address,
	// self-IPs) lives in the network-design-checklist.md the binary
	// dropped into the PoC repo. Run validate to print the punch list
	// — operator sees exactly what's still missing before provisioning.
	fmt.Fprintln(out, "\nNext steps:")
	checklist := filepath.Join(repo, "network-design-checklist.md")
	if _, err := os.Stat(checklist); err == nil {
		fmt.Fprintf(out, "  1. Walk %s with the customer (LAG, VLANs, IPs, MTU, self-IPs).\n", checklist)
		fmt.Fprintln(out, "     Record each answer in poc.yaml and the rationale in decisions.md.")
	} else {
		fmt.Fprintln(out, "  1. Edit poc.yaml: set per-DPU VLANs + IPs, MTU, cluster apiserver address.")
	}
	fmt.Fprintln(out, "  2. Drop FAR tarball + JWT + DPU password hash into keys/.")
	fmt.Fprintln(out, "  3. Run `dpubnkctl validate` to confirm poc.yaml is consistent.")
	fmt.Fprintln(out, "  4. `dpubnkctl provision dpus <hostname> --yolo --confirm-flash <hostname>`.")

	fmt.Fprintln(out, "\n--- dpubnkctl validate ---")
	vr := poc.Validate(p, repo)
	printValidation(out, vr)
	if total := len(vr.Errors) + len(vr.Warnings); total > 0 {
		// Errors block phase commands outright; warnings are deferred
		// errors (e.g. missing self-IPs that `deploy cne` will trip on).
		// Lump them in the same count so the operator sees the full
		// punch list, not just the blocking subset.
		fmt.Fprintf(out, "\n%d issue(s) still need attention before provisioning (%d error(s), %d warning(s)). Discovery itself completed successfully.\n",
			total, len(vr.Errors), len(vr.Warnings))
	}
	return nil
}

// suggestRole picks the default role offered for the per-host prompt
// and a short rationale string the wizard prints alongside, so the
// operator (or an agentic SE persona reading the same output) sees the
// *reason* the default was suggested, not just the label.
//
// Inputs: this host's DPU count, total reachable hosts, count of
// DPU-free hosts among the reachable set.
//
//   - Host without DPU       → control-plane  (no data plane to host)
//   - Host with DPU(s) and
//     ≥3 DPU-free hosts      → worker         (dedicated CPs available
//                                              for HA quorum)
//   - Host with DPU(s) and
//     <3 DPU-free hosts and
//     totalReachable == 1    → both           (single-host lab)
//   - Host with DPU(s) and
//     <3 DPU-free hosts      → both           (DPU host must also serve
//                                              as CP since no DPU-free
//                                              host can carry the CP role
//                                              alone for HA)
//
// Operators can override per host — the suggestion just biases the
// default and explains why.
// Kubernetes control-plane HA needs an odd number of nodes (1 for a lab,
// 3+ for HA — etcd quorum). suggestRole uses 3 as the dedicated-CP
// threshold below.
func suggestRole(dpuCount, totalReachable, noDPUHosts int) (role, rationale string) {
	if dpuCount == 0 {
		return "control-plane", "no DPU → no data-plane work; ideal control-plane-only candidate"
	}
	if noDPUHosts >= 3 {
		return "worker", fmt.Sprintf("has DPU(s); %d DPU-free hosts available as dedicated control planes → this host is worker only", noDPUHosts)
	}
	if totalReachable == 1 {
		return "both", "single-host lab — must run control plane and worker on the same node"
	}
	return "both", fmt.Sprintf("has DPU(s); only %d DPU-free host(s) — fewer than the 3 control-plane-only nodes etcd needs for HA, so this host runs control plane and worker", noDPUHosts)
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

// askPort prompts for a TCP port. Re-prompts on unparseable or out-of-
// range input rather than silently falling through to the default,
// since a typo (e.g. "2222!") followed by the default would silently
// scan port 22 against a non-22 lab and exit 0.
func askPort(out io.Writer, r *bufio.Reader, label, hint string, def int) int {
	for {
		v := ask(out, r, label, hint, fmt.Sprintf("%d", def))
		if v == "" {
			return def
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 65535 {
			fmt.Fprintf(out, "  ! invalid port %q — must be an integer in 1..65535\n", v)
			continue
		}
		return n
	}
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

// fillDPUWizardDefaults walks p.Hosts and, for the host whose SSH address
// matches addr, fills any empty DPU.Hostname with `<host>-bf3` and any
// empty DPU.TmfifoIP with 192.168.100.2/30 (the host↔DPU rshim link is
// per-host private, so every DPU can reuse the same /30 — matching the
// pattern already in examples/two-node-homelab.yaml and the homelab).
//
// Empty-only: never clobbers anything the operator (or a prior wizard
// run) has already set. For multi-DPU hosts the second+ DPU sticks with
// the same value as well; multi-DPU is rare and the operator can adjust.
func fillDPUWizardDefaults(p *poc.PoC, addr, hostName string) {
	for i := range p.Hosts {
		if p.Hosts[i].SSH.Address != addr {
			continue
		}
		for j := range p.Hosts[i].DPUs {
			if p.Hosts[i].DPUs[j].Hostname == "" {
				p.Hosts[i].DPUs[j].Hostname = hostName + "-bf3"
			}
			if p.Hosts[i].DPUs[j].TmfifoIP == "" {
				p.Hosts[i].DPUs[j].TmfifoIP = "192.168.100.2/30"
			}
		}
		return
	}
}
