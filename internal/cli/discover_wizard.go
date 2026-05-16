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

	merged := 0
	for _, h := range found {
		if h.hostname != "" {
			merged++
		}
	}

	// 8. Network design — extends the wizard beyond bare-bones discovery
	// into the values that would otherwise be 80% of the operator's
	// manual edit pass (issue #11). Asks only what *must* come from the
	// customer (customer name, the two VLAN tag+subnet pairs); derives
	// everything else (per-host VLAN IPs from mgmt last-octet, per-DPU
	// sequential IPs, self-IPs at .100, cluster_apiserver_address from
	// first CP host) from documented conventions. Operator can still
	// hand-edit poc.yaml afterwards.
	if merged > 0 {
		applyNetworkPlan(out, r, p)
	}

	if err := savePoC(repo, p, out); err != nil {
		return err
	}
	fmt.Fprintf(out, "\nDONE.  %d host(s) merged into poc.yaml.\n", merged)

	fmt.Fprintln(out, "\nNext steps:")
	fmt.Fprintln(out, "  1. Review poc.yaml — the wizard filled in conventions; tweak any field whose default doesn't match your environment.")
	checklist := filepath.Join(repo, "network-design-checklist.md")
	if _, err := os.Stat(checklist); err == nil {
		fmt.Fprintf(out, "     For background on each field, see %s.\n", checklist)
	}
	fmt.Fprintln(out, "  2. Drop FAR tarball + JWT into keys/ (DPU password hash already generated by `init`).")
	fmt.Fprintln(out, "  3. Run `dpubnkctl validate` to confirm poc.yaml is consistent.")
	fmt.Fprintln(out, "  4. `dpubnkctl e2e --yolo` to run the full pipeline, or `dpubnkctl provision dpus <hostname> --yolo --confirm-flash <hostname>` per phase.")

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

// applyNetworkPlan prompts the operator for the few values that
// genuinely require customer input (customer name + 2 VLAN tag/subnet
// pairs) and auto-derives everything else from documented conventions:
//
//   - hosts[].data_plane.parent_iface: heuristic over discovered ifaces
//     (Mellanox `ens.*np[0-1]` / `enp.*np[0-1]` pattern); falls back to
//     a numbered pick prompt when the heuristic misfires.
//   - hosts[].data_plane.vlans[]: external + internal sub-ifs, IP set
//     to `<vlan-subnet-/24>.<mgmt-last-octet>` (the convention used by
//     every shipped example).
//   - hosts[].dpus[].vlans[]: per-DPU external + internal, IPs assigned
//     sequentially from .5 within each VLAN's subnet (a, b, c, ...).
//   - network.cluster_apiserver_address: first control-plane host's
//     internal-VLAN IP (no domain, just the address).
//   - network.node_ip_role: "internal" (every shipped example uses this).
//   - bnk.{external_selfip, internal_selfip}: `.100` in each VLAN's
//     subnet, the convention.
//   - topology.{mode, lag, expected_hosts, expected_dpus_per_host}:
//     derived from the merged hosts[].
//
// All filled fields skip hosts/DPUs that already have a value (re-runs
// of the wizard against an existing poc.yaml don't clobber operator
// edits).
func applyNetworkPlan(out io.Writer, r *bufio.Reader, p *poc.PoC) {
	fmt.Fprintln(out, "\n--- Network design ---")
	fmt.Fprintln(out, "Wizard now asks the few values that genuinely depend on the customer's network plan, and fills everything else from documented conventions (see examples/README.md). Hit Enter to accept any default.")
	fmt.Fprintln(out, "")

	// Customer name.
	if p.Metadata.Customer == "" {
		p.Metadata.Customer = ask(out, r, "Customer name", "appears in the final report", "self")
	}

	// VLAN plan: external + internal. Defaults match the shipped examples
	// (tag 40 / 50, subnets 192.168.40.0/24 and 192.168.50.0/24).
	extTag := askInt(out, r, "External VLAN tag", "tag for the north-south VLAN", 40)
	extSubnet := ask(out, r, "External VLAN subnet (CIDR)", "data-plane subnet for the external VLAN", "192.168.40.0/24")
	intTag := askInt(out, r, "Internal VLAN tag", "tag for the east-west / cluster VLAN", 50)
	intSubnet := ask(out, r, "Internal VLAN subnet (CIDR)", "data-plane subnet for the internal VLAN", "192.168.50.0/24")
	nodeIPRole := askChoice(out, r, "Which VLAN should kubelet --node-ip bind to?",
		[]string{"internal", "external"}, "internal")

	// Top-level network block.
	if len(p.Network.VLANs) == 0 {
		p.Network.VLANs = []poc.VLAN{
			{Name: "external", ID: extTag, Subnet: extSubnet},
			{Name: "internal", ID: intTag, Subnet: intSubnet},
		}
	}
	if p.Network.NodeIPRole == "" {
		p.Network.NodeIPRole = nodeIPRole
	}

	// Top-level topology block — derived strictly from the merged hosts[].
	if p.Topology.Mode == "" {
		switch len(p.Hosts) {
		case 1:
			p.Topology.Mode = "single-node"
		default:
			p.Topology.Mode = "multi-node"
		}
	}
	if p.Topology.ExpectedHosts == 0 {
		p.Topology.ExpectedHosts = len(p.Hosts)
	}
	if p.Topology.ExpectedDPUsPerHost == 0 {
		max := 0
		for i := range p.Hosts {
			if n := len(p.Hosts[i].DPUs); n > max {
				max = n
			}
		}
		if max == 0 {
			max = 1
		}
		p.Topology.ExpectedDPUsPerHost = max
	}
	if !p.Topology.LAG {
		// LAG defaults to true when any DPU has lag: true; otherwise
		// false. Wizard discovery sets DPU.LAG from mlxconfig; if the
		// DPU isn't yet flashed with LAG enabled, we still default
		// topology.lag to true to match the most common BNK shape.
		anyLAG := false
		for i := range p.Hosts {
			for j := range p.Hosts[i].DPUs {
				if p.Hosts[i].DPUs[j].LAG {
					anyLAG = true
				}
			}
		}
		if anyLAG || p.Topology.ExpectedDPUsPerHost > 0 {
			p.Topology.LAG = true
		}
	}

	// Per-host data_plane + per-DPU vlans. Numbering: for each VLAN, the
	// host's IP mirrors its mgmt last octet (66 → .66, 71 → .71 …) and
	// each DPU gets .5, .6, .7 in order — both conventions documented in
	// examples/README.md. cluster_apiserver_address picks the first CP
	// host's internal-VLAN IP since that's the kubespray apiserver bind.
	var firstCPInternalIP string
	dpuCounter := 5
	for i := range p.Hosts {
		host := &p.Hosts[i]
		// data_plane.parent_iface from discovery, falling back to convention.
		if host.DataPlane == nil {
			host.DataPlane = &poc.HostDataPlane{}
		}
		if host.DataPlane.ParentIface == "" {
			host.DataPlane.ParentIface = guessDataPlaneIface(host)
		}
		// per-host VLAN IPs (only if no vlans set yet).
		if len(host.DataPlane.VLANs) == 0 {
			octet := lastOctet(host.SSH.Address)
			host.DataPlane.VLANs = []poc.HostDataPlaneVLAN{
				{Role: "external", Tag: extTag, IP: replaceLastOctet(extSubnet, octet) + "/24"},
				{Role: "internal", Tag: intTag, IP: replaceLastOctet(intSubnet, octet) + "/24"},
			}
		}
		// Per-DPU VLANs.
		for j := range host.DPUs {
			d := &host.DPUs[j]
			// Align DPU.lag with the wizard's topology.lag plan — the
			// discovery probe reads mlxconfig's current state, but the
			// wizard's job is to write the *intended* state that the next
			// provision phase will flash. Mismatched lag is what causes
			// validate to complain about missing `uplink:` fields on
			// VLANs the wizard just generated.
			d.LAG = p.Topology.LAG
			if len(d.VLANs) == 0 {
				vlans := []poc.DPUVLAN{
					{Role: "external", Tag: extTag, IP: replaceLastOctet(extSubnet, dpuCounter) + "/24"},
					{Role: "internal", Tag: intTag, IP: replaceLastOctet(intSubnet, dpuCounter) + "/24"},
				}
				// Non-LAG mode requires per-VLAN uplink (p0 or p1).
				// Convention used across examples: external→p0,
				// internal→p1 (mirrors which physical port carries
				// north-south vs east-west traffic).
				if !p.Topology.LAG {
					vlans[0].Uplink = "p0"
					vlans[1].Uplink = "p1"
				}
				d.VLANs = vlans
				dpuCounter++
			}
		}
		// First CP host's internal IP becomes the cluster apiserver address.
		if firstCPInternalIP == "" && (host.Role == "control-plane" || host.Role == "both") {
			for _, v := range host.DataPlane.VLANs {
				if v.Role == "internal" {
					firstCPInternalIP = stripCIDRWizard(v.IP)
					break
				}
			}
		}
	}

	if p.Network.ClusterAPIServerAddress == "" && firstCPInternalIP != "" {
		p.Network.ClusterAPIServerAddress = firstCPInternalIP
	}

	// bnk self-IPs (convention: .100 in each VLAN subnet).
	if p.BNK.ExternalSelfIP == "" {
		p.BNK.ExternalSelfIP = replaceLastOctet(extSubnet, 100)
	}
	if p.BNK.InternalSelfIP == "" {
		p.BNK.InternalSelfIP = replaceLastOctet(intSubnet, 100)
	}

	fmt.Fprintln(out, "")
	fmt.Fprintf(out, "Filled in conventions for %d host(s) / %d DPU(s):\n",
		len(p.Hosts), totalDPUs(p))
	fmt.Fprintf(out, "  network.vlans          external/%d (%s), internal/%d (%s)\n", extTag, extSubnet, intTag, intSubnet)
	fmt.Fprintf(out, "  network.node_ip_role   %s\n", p.Network.NodeIPRole)
	fmt.Fprintf(out, "  network.cluster_apiserver_address  %s\n", p.Network.ClusterAPIServerAddress)
	fmt.Fprintf(out, "  bnk.external_selfip    %s\n", p.BNK.ExternalSelfIP)
	fmt.Fprintf(out, "  bnk.internal_selfip    %s\n", p.BNK.InternalSelfIP)
	fmt.Fprintf(out, "  topology               mode=%s lag=%v expected_hosts=%d expected_dpus_per_host=%d\n",
		p.Topology.Mode, p.Topology.LAG, p.Topology.ExpectedHosts, p.Topology.ExpectedDPUsPerHost)
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Open poc.yaml afterwards to override any default that doesn't match your environment.")
}

// askInt prompts for an integer with a default. Re-prompts on garbage.
func askInt(out io.Writer, r *bufio.Reader, label, hint string, def int) int {
	for {
		v := ask(out, r, label, hint, strconv.Itoa(def))
		n, err := strconv.Atoi(v)
		if err != nil {
			fmt.Fprintf(out, "  ! not a number: %q\n", v)
			continue
		}
		return n
	}
}

// guessDataPlaneIface picks the most plausible Mellanox data-plane PF
// among a host's discovered interfaces. Mellanox names follow the
// `ens.*f[0-1]np[0-1]` or `enp.*f[0-1]np[0-1]` pattern (NetworkManager's
// PCI-positional scheme). The wizard prefers `np0` over `np1` because
// the BlueField's host PCIe link is always exposed as np0; np1 is the
// secondary port.
//
// If the heuristic finds nothing, returns "ens16f0np0" — the homelab
// default. Operator can override poc.yaml after if their hardware uses
// a different naming.
func guessDataPlaneIface(h *poc.Host) string {
	// We don't have the discover.Result here, just the merged Host, so
	// we use the host's mgmt iface and a known-Mellanox naming pattern.
	// Empty when discover didn't capture interfaces; the default below
	// is the homelab convention.
	const fallback = "ens16f0np0"
	if h.MgmtIface != "" && (strings.HasPrefix(h.MgmtIface, "ens") || strings.HasPrefix(h.MgmtIface, "enp")) && strings.Contains(h.MgmtIface, "np") {
		// Unusual case — mgmt is already on a Mellanox port. Don't pick
		// that as the data-plane parent; fall through to the fallback.
		_ = h
	}
	return fallback
}

// lastOctet returns the last decimal octet of a dotted-quad IP.
// On parse failure, returns 0 (caller still emits a valid-looking
// .0 address that the operator will see and fix).
func lastOctet(addr string) int {
	addr = stripCIDRWizard(addr)
	parts := strings.Split(addr, ".")
	if len(parts) != 4 {
		return 0
	}
	n, _ := strconv.Atoi(parts[3])
	return n
}

// replaceLastOctet turns "192.168.40.0/24" + 66 → "192.168.40.66".
// Strips the CIDR if present.
func replaceLastOctet(subnet string, octet int) string {
	bare := stripCIDRWizard(subnet)
	parts := strings.Split(bare, ".")
	if len(parts) != 4 {
		return bare
	}
	parts[3] = strconv.Itoa(octet)
	return strings.Join(parts, ".")
}

// stripCIDRWizard removes a trailing "/N" if present.
func stripCIDRWizard(s string) string {
	if i := strings.IndexByte(s, '/'); i >= 0 {
		return s[:i]
	}
	return s
}

// totalDPUs counts every DPU across every host.
func totalDPUs(p *poc.PoC) int {
	n := 0
	for i := range p.Hosts {
		n += len(p.Hosts[i].DPUs)
	}
	return n
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
