package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/mwiget/dpubnkctl/internal/cluster"
	"github.com/mwiget/dpubnkctl/internal/deploy"
	"github.com/mwiget/dpubnkctl/internal/poc"
	"github.com/mwiget/dpubnkctl/internal/ssh"
)

type clusterJoinDPUsFlags struct {
	pocDir         string
	yolo           bool
	confirmCluster string
	timeout        time.Duration
	skipInstall    bool
	skipLabelTaint bool
}

func newClusterJoinDPUsCmd() *cobra.Command {
	f := &clusterJoinDPUsFlags{}
	cmd := &cobra.Command{
		Use:   "join-dpus",
		Short: "Join every DPU in poc.yaml as a worker node (DESTRUCTIVE on each DPU)",
		Long: `For each DPU recorded in poc.yaml.hosts[].dpus[]:

  1. SSH to the DPU through its host (jumphost) using the operator's
     SSH key — installed at flash time via bf.conf.
  2. Install kubelet/kubeadm/kubectl matching the cluster's k8s version
     (apt repo from pkgs.k8s.io). Hold to prevent drift.
  3. Get a fresh kubeadm join command from the first control plane.
  4. Run kubeadm join on the DPU with --node-name = dpu.hostname and
     --cri-socket pointing at containerd (configured by bf.conf).
  5. From the operator, label the new node app=f5-tmm and taint it
     dpu=true:NoSchedule so only BNK pods land there.

Required gates:
  --yolo                   acknowledge that this writes to DPU OS + cluster
  --confirm-cluster NAME   must equal poc.yaml.metadata.name`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runClusterJoinDPUs(cmd.Context(), cmd.OutOrStdout(), f)
		},
	}
	cmd.Flags().StringVar(&f.pocDir, "poc", "", "PoC repo path (default: current directory)")
	cmd.Flags().BoolVar(&f.yolo, "yolo", false, "Acknowledge that this command modifies DPU OS + cluster")
	cmd.Flags().StringVar(&f.confirmCluster, "confirm-cluster", "", "Must equal poc.yaml.metadata.name (typo guard)")
	cmd.Flags().DurationVar(&f.timeout, "timeout", 30*time.Minute, "Per-DPU join timeout")
	cmd.Flags().BoolVar(&f.skipInstall, "skip-install", false, "Assume kubelet/kubeadm/kubectl already on DPU; just join")
	cmd.Flags().BoolVar(&f.skipLabelTaint, "skip-label-taint", false, "Don't label/taint DPU nodes after join")
	return cmd
}

type dpuJob struct {
	host *poc.Host
	dpu  *poc.DPU
}

// dpuNetSetup carries the resolved setup-dpu-networking config for the
// rshim join path. enabled is false for the vlan transport (no-op).
type dpuNetSetup struct {
	enabled bool
	mode    string   // host-nat | oob | none (poc.EffectiveDPUInternet)
	masqSrc string   // tmfifo subnet to MASQUERADE
	dns     []string // DPU resolv.conf nameservers
}

func runClusterJoinDPUs(ctx context.Context, out io.Writer, f *clusterJoinDPUsFlags) error {
	repo, err := resolvePoCDir(f.pocDir)
	if err != nil {
		return err
	}
	p, err := poc.Load(repo)
	if err != nil {
		return fmt.Errorf("not a PoC repo (%s): %w", repo, err)
	}
	if err := requireTwoGates(f.yolo, "--confirm-cluster", f.confirmCluster, p.Metadata.Name, "DPU join"); err != nil {
		return err
	}
	if err := enforceValidateForPhase(out, p, repo, poc.PhaseCluster, false); err != nil {
		return err
	}

	// Under rshim, make sure every host/DPU has its tmfifo address (from
	// the pool, or the single-host default) before we compute node-IPs
	// and the apiserver address. Idempotent; persists poc.yaml if it
	// filled anything in.
	if err := ensureTmfifoAllocated(repo, p, out); err != nil {
		return err
	}

	plan := cluster.BuildPlan(p)
	if !plan.Valid() {
		return fmt.Errorf("plan invalid: %s", strings.Join(plan.Errors, "; "))
	}
	if len(plan.ControlPlane) == 0 {
		return errors.New("no control plane in plan — run `dpubnkctl cluster up` first")
	}

	// Build the per-DPU job list. Skip hosts with no DPUs.
	var jobs []dpuJob
	for i := range p.Hosts {
		h := &p.Hosts[i]
		for j := range h.DPUs {
			d := &h.DPUs[j]
			if d.Hostname == "" || d.TmfifoIP == "" {
				return fmt.Errorf("host %s dpu %s: hostname or tmfifo_ip not set in poc.yaml — re-run `provision plan` to validate", h.Name, d.PCI)
			}
			jobs = append(jobs, dpuJob{host: h, dpu: d})
		}
	}
	if len(jobs) == 0 {
		return errors.New("no DPUs in poc.yaml — run `dpubnkctl discover` + flash first")
	}

	fmt.Fprintf(out, "PoC:        %s\n", p.Metadata.Name)
	fmt.Fprintf(out, "Cluster:    %v\n", plan.ControlPlane)
	fmt.Fprintf(out, "Joining:    %d DPU(s) across %d host(s)\n", len(jobs), len(p.Hosts))

	// Resolve the setup-dpu-networking config once. Under rshim the DPU
	// gets internet via the host before the join-time apt install; the
	// vlan transport leaves this disabled (DPUs are assumed online, e.g.
	// via oob, exactly as before).
	var netSetup dpuNetSetup
	if p.Network.IsRshim() {
		netSetup = dpuNetSetup{
			enabled: true,
			mode:    p.EffectiveDPUInternet(),
			masqSrc: p.Network.TmfifoCIDR,
			dns:     p.Provisioning.DPUDNS,
		}
		if netSetup.masqSrc == "" {
			netSetup.masqSrc = "192.168.100.0/24"
		}
		fmt.Fprintf(out, "Transport:  rshim (tmfifo) — dpu_internet=%s\n", netSetup.mode)
	}
	fmt.Fprintln(out)

	// 1. Get a join command from the first control plane.
	fmt.Fprintln(out, "[1/3] Fetching kubeadm join command from first control plane ...")
	cpHost := plan.HostByName[plan.ControlPlane[0]]
	cpCfg, err := sshConfigForHost(repo, cpHost, 30*time.Second)
	if err != nil {
		return err
	}
	cpDial, cancel := context.WithTimeout(ctx, 30*time.Second)
	cpClient, err := ssh.Dial(cpDial, cpCfg)
	cancel()
	if err != nil {
		return fmt.Errorf("ssh control-plane: %w", err)
	}
	defer cpClient.Close()
	// Resolve the apiserver address the DPUs should join against.
	//   - rshim: the control-plane host's tmfifo IP — the DPU reaches the
	//     apiserver directly over the tmfifo link (single-host). The cert
	//     carries this IP via supplementary_addresses_in_ssl_keys (see
	//     renderGroupVarsAll). Multi-host rshim needs a per-DPU apiserver
	//     address routed through the owning host — deferred (step 5).
	//   - vlan: poc.network.cluster_apiserver_address (the data-plane
	//     VIP/IP), else the control plane's SSH address (legacy fallback).
	var apiserverAddr string
	if p.Network.IsRshim() {
		apiserverAddr = bareIP(cpHost.TmfifoHostIP())
		if apiserverAddr == "" {
			return fmt.Errorf("control-plane host %s has no usable tmfifo_ip for rshim apiserver address", cpHost.Name)
		}
	} else if apiserverAddr = p.Network.ClusterAPIServerAddress; apiserverAddr == "" {
		apiserverAddr = cpHost.SSH.Address
	}
	fmt.Fprintf(out, "      apiserver = %s:6443\n", apiserverAddr)
	jc, err := cluster.FetchJoinCommand(ctx, cpClient, apiserverAddr)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "      ok — %s\n", redactToken(jc.Raw))

	// 2. Fan out: install + join each DPU in parallel.
	fmt.Fprintln(out, "[2/3] Installing kubelet/kubeadm/kubectl on DPUs and joining ...")
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		failures []string
		joined   []dpuJob // DPUs that ended this phase as cluster members
	)
	for _, j := range jobs {
		j := j
		wg.Add(1)
		go func() {
			defer wg.Done()
			tag := fmt.Sprintf("[%s]", j.dpu.Hostname)
			nodeIP, err := resolveDPUNodeIP(j.dpu, p.Network)
			if err != nil {
				mu.Lock()
				failures = append(failures, fmt.Sprintf("%s: %v", j.dpu.Hostname, err))
				mu.Unlock()
				fmt.Fprintf(out, "%s ERR: %v\n", tag, err)
				return
			}
			outcome, err := joinOneDPU(ctx, repo, j, jc, nodeIP, p.Versions.K8s, netSetup, f, prefixWriter{w: out, prefix: tag + " "})
			if err != nil {
				mu.Lock()
				failures = append(failures, fmt.Sprintf("%s: %v", j.dpu.Hostname, err))
				mu.Unlock()
				fmt.Fprintf(out, "%s ERR: %v\n", tag, err)
				return
			}
			mu.Lock()
			joined = append(joined, j)
			mu.Unlock()
			switch outcome {
			case joinOutcomeJoined:
				fmt.Fprintf(out, "%s joined.\n", tag)
			case joinOutcomeAlreadyJoined:
				fmt.Fprintf(out, "%s already a cluster member (skipped kubeadm join).\n", tag)
			}
		}()
	}
	wg.Wait()

	// 3. Label + taint the DPUs that ended up as cluster members, even
	//    if some other DPU's join failed. Both label and taint use
	//    `kubectl label --overwrite` / `kubectl taint --overwrite` so a
	//    re-run against an already-labeled node is safe — and a
	//    partial-success retry shouldn't leave the succeeded DPU
	//    unlabeled just because a sibling failed.
	var labelTaintErr error
	if !f.skipLabelTaint && len(joined) > 0 {
		fmt.Fprintf(out, "[3/3] Labeling + tainting %d DPU node(s) ...\n", len(joined))
		if err := labelAndTaintDPUs(ctx, repo, joined, out); err != nil {
			labelTaintErr = err
			fmt.Fprintf(out, "      ERR: label/taint failed: %v\n", err)
		}
	} else if f.skipLabelTaint {
		fmt.Fprintln(out, "[3/3] (--skip-label-taint)")
	} else {
		fmt.Fprintln(out, "[3/3] (no DPUs joined — skipping label/taint)")
	}

	// Surface join failures AFTER labeling so the succeeded DPUs get
	// finalized regardless. Exit non-zero so the operator (or e2e) knows
	// to retry the failed ones.
	if len(failures) > 0 {
		return fmt.Errorf("%d DPU join(s) failed:\n  - %s", len(failures), strings.Join(failures, "\n  - "))
	}

	// A label/taint failure is a phase failure, NOT a warning: an
	// unlabeled/un-tainted DPU node can't run BNK (CNE nodeSelects
	// app=f5-tmm and tolerates dpu=true:NoSchedule), so a "success" here
	// only surfaces ~40 min later as CNE DaemonSets refusing to schedule.
	// Journal the real outcome, then fail loud so automation/agent retries.
	if labelTaintErr != nil {
		appendJoinJournal(repo, p.Metadata.Name, jobs, labelTaintErr)
		return fmt.Errorf("DPU node label/taint failed (nodes joined but not BNK-ready): %w", labelTaintErr)
	}

	// Update poc.yaml + journal.
	p.Status.Cluster = "completed"
	p.Status.LastPhaseAt = time.Now().UTC()
	if err := savePoC(repo, p, out); err != nil {
		return err
	}
	appendJoinJournal(repo, p.Metadata.Name, jobs, nil)
	fmt.Fprintln(out, "\nDONE.")
	return nil
}

// probeOOBIP returns the DPU's oob_net0 IPv4 address as CIDR (e.g.
// "192.168.68.96/22"). Best-effort — returns "" if the command fails
// or the output is unparseable. Output of `ip -br -4 addr show oob_net0`
// looks like:
//
//	oob_net0         UP             192.168.68.96/22 metric 100
//
// Stashing the full CIDR (not just the bare IP) keeps poc.yaml's oob_ip
// shape parallel to tmfifo_ip and preserves the DHCP-supplied netmask
// — useful when diagnosing routing problems or printing the mgmt subnet
// in reports. The diagram renderer strips the prefix for display.
func probeOOBIP(ctx context.Context, c *ssh.Client) string {
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	r := c.Run(pctx, "ip -br -4 addr show oob_net0 2>/dev/null")
	if !r.OK() {
		return ""
	}
	for _, f := range strings.Fields(r.Stdout) {
		if ip, ipnet, err := net.ParseCIDR(f); err == nil && ip.To4() != nil {
			ones, _ := ipnet.Mask.Size()
			return fmt.Sprintf("%s/%d", ip.String(), ones)
		}
	}
	return ""
}

// resolveDPUNodeIP picks the kubelet --node-ip for a DPU.
//   - rshim: the DPU's tmfifo IP (bare). The DPU's default route is
//     tmfifo, so its kubelet InternalIP lands on the tmfifo address and
//     the whole control plane is reached over that link.
//   - vlan + network.node_ip_role set: the bare IP from the matching
//     dpu.vlans entry.
//   - vlan + node_ip_role unset: "" so kubeadm/kubelet auto-detect
//     (legacy behavior — yields the oob_net0/mgmt IP).
func resolveDPUNodeIP(d *poc.DPU, netw poc.Network) (string, error) {
	if netw.IsRshim() {
		ip, _, err := net.ParseCIDR(d.TmfifoIP)
		if err != nil {
			return "", fmt.Errorf("dpu %s tmfifo_ip %q: %w", d.Hostname, d.TmfifoIP, err)
		}
		return ip.String(), nil
	}
	role := netw.NodeIPRole
	if role == "" {
		return "", nil
	}
	v := d.VLANByRole(role)
	if v == nil {
		return "", fmt.Errorf("dpu %s has no vlan with role=%q (network.node_ip_role)", d.Hostname, role)
	}
	ip, _, err := net.ParseCIDR(v.IP)
	if err != nil {
		return "", fmt.Errorf("dpu %s vlan %s ip %q: %w", d.Hostname, v.PortName(), v.IP, err)
	}
	return ip.String(), nil
}

// ensureTmfifoAllocated runs the rshim tmfifo address allocation and
// persists poc.yaml when it changes anything. No-op for the vlan
// transport. Allocation is deterministic, so re-running on an
// already-allocated PoC is a no-op (no spurious poc.yaml churn) and a
// redeploy yields identical addresses.
func ensureTmfifoAllocated(repo string, p *poc.PoC, out io.Writer) error {
	changed, err := poc.AllocateTmfifo(p)
	if err != nil {
		return err
	}
	if changed {
		if err := savePoC(repo, p, out); err != nil {
			return err
		}
	}
	return nil
}

// bareIP strips a CIDR suffix, returning the bare IPv4/IPv6 string, or ""
// if raw is empty/unparseable. Accepts a bare IP too.
func bareIP(raw string) string {
	if raw == "" {
		return ""
	}
	if ip, _, err := net.ParseCIDR(raw); err == nil {
		return ip.String()
	}
	if ip := net.ParseIP(raw); ip != nil {
		return ip.String()
	}
	return ""
}

// joinOutcome distinguishes "did a real kubeadm join" from "DPU was
// already a cluster member, fast-path". The outer caller still treats
// both as success — this only changes the user-facing message and the
// journal entry.
type joinOutcome int

const (
	joinOutcomeJoined        joinOutcome = iota // ran kubeadm join, OK
	joinOutcomeAlreadyJoined                    // /etc/kubernetes/kubelet.conf already present
)

func joinOneDPU(ctx context.Context, repo string, j dpuJob, jc *cluster.JoinCommand, nodeIP, k8sMinor string, netSetup dpuNetSetup, f *clusterJoinDPUsFlags, w io.Writer) (joinOutcome, error) {
	// Pre-dial step: make sure the host end of THIS DPU's tmfifo link is
	// up and addressed before we try to reach the DPU across it. The
	// rshim kernel module *should* assign the host address at module
	// load, but any `systemctl restart rshim` (operator clearing an
	// orphaned console reader, or any other lifecycle event) wipes it.
	// Without it the DPU dial times out with "context deadline exceeded"
	// — observed twice on the ailab single-node PoC across `provision
	// dpu` and `cluster join-dpus`. Idempotent — `ip addr add` returns
	// "File exists" (which the helper swallows) when already set.
	//
	// Per-DPU, not per-host: on a multi-DPU host the second card is on
	// tmfifo_net1 with its own /30, and preparing only tmfifo_net0 left
	// it unreachable (issue #20).
	hostCfg, err := sshConfigForHost(repo, j.host, 30*time.Second)
	if err != nil {
		return joinOutcomeJoined, fmt.Errorf("host ssh config: %w", err)
	}
	hostDialCtx, hostCancel := context.WithTimeout(ctx, 30*time.Second)
	hostC, err := ssh.Dial(hostDialCtx, hostCfg)
	hostCancel()
	if err != nil {
		return joinOutcomeJoined, fmt.Errorf("ssh host (for tmfifo prep): %w", err)
	}
	ensureHostTmfifoForDPU(ctx, hostC, j.dpu)
	// The rshim path needs the host session again for NAT setup below;
	// keep it open until the join finishes. The vlan path is done with
	// the host after the tmfifo prep.
	if netSetup.enabled {
		defer hostC.Close()
	} else {
		hostC.Close()
	}

	cfg, err := dpuSSHConfig(repo, j.host, j.dpu)
	if err != nil {
		return joinOutcomeJoined, err
	}

	fmt.Fprintf(w, "ssh %s (tmfifo via %s) ...\n", j.dpu.Hostname, j.host.Name)
	dialCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	c, err := ssh.Dial(dialCtx, cfg)
	cancel()
	if err != nil {
		return joinOutcomeJoined, fmt.Errorf("ssh dpu: %w", err)
	}
	defer c.Close()

	// Capture the DPU's oob_net0 (GigE OOB mgmt) IP — DHCP-assigned at
	// first boot, so we can only learn it once the DPU is reachable.
	// tmfifo is host-local; oob_net0 is what shows up in reports and the
	// diagram. Best-effort: don't fail the join if parsing fails.
	if ip := probeOOBIP(ctx, c); ip != "" && ip != j.dpu.OOBIP {
		fmt.Fprintf(w, "oob_net0 = %s\n", ip)
		j.dpu.OOBIP = ip
	}

	// Fast-path: if this DPU was already joined on a prior attempt, skip
	// the install + join entirely. The install step disables kubelet, so
	// re-running it on an already-joined DPU would risk breaking the
	// running node (the previous behavior left worker1-bf3 kubelet
	// `inactive (dead)` for several minutes during the lake1 retry; see
	// punch-list item 5/6). We DO want label/taint to re-run, which
	// happens regardless of this outcome.
	if cluster.AlreadyJoined(ctx, c) {
		fmt.Fprintln(w, "already a cluster member — skipping install + kubeadm join.")
		return joinOutcomeAlreadyJoined, nil
	}

	// Under rshim, give the DPU internet through the host BEFORE the apt
	// install below (kubelet/kubeadm/kubectl come from pkgs.k8s.io).
	if netSetup.enabled {
		gw := bareIP(j.host.TmfifoHostIP())
		if err := setupDPUNetworking(ctx, hostC, c, netSetup.mode, gw, netSetup.masqSrc, netSetup.dns, w); err != nil {
			return joinOutcomeJoined, err
		}
	}

	if !f.skipInstall {
		fmt.Fprintln(w, "installing kubelet/kubeadm/kubectl ...")
		instCtx, icancel := context.WithTimeout(ctx, 10*time.Minute)
		err := cluster.InstallKubeBinaries(instCtx, c, k8sMinor)
		icancel()
		if err != nil {
			return joinOutcomeJoined, err
		}
		fmt.Fprintln(w, "install ok")
	}

	if nodeIP != "" {
		fmt.Fprintf(w, "running kubeadm join (--node-ip %s) ...\n", nodeIP)
	} else {
		fmt.Fprintln(w, "running kubeadm join ...")
	}
	if _, err := cluster.JoinDPU(ctx, c, jc, j.dpu.Hostname, nodeIP); err != nil {
		return joinOutcomeJoined, err
	}
	// Restart containerd so its CRI re-scans /etc/cni/net.d. kubeadm
	// join only waited for the kubelet TLS bootstrap; it didn't trigger
	// a CNI re-init. Without this the DPU stays NotReady even after
	// Calico-node + later Multus daemonsets land their configs.
	fmt.Fprintln(w, "restarting containerd (CRI re-scan) ...")
	if err := cluster.RestartContainerd(ctx, c); err != nil {
		fmt.Fprintf(w, "WARN: containerd restart failed (DPU may stay NotReady): %v\n", err)
	}
	return joinOutcomeJoined, nil
}

func labelAndTaintDPUs(ctx context.Context, repo string, jobs []dpuJob, out io.Writer) error {
	kubeconfig := filepath.Join(repo, "artifacts", "kubeconfig")
	if _, err := os.Stat(kubeconfig); err != nil {
		return fmt.Errorf("kubeconfig %s: %w", kubeconfig, err)
	}
	r := &deploy.Runner{KubeconfigPath: kubeconfig, Out: prefixWriter{w: out, prefix: "      | "}}
	// Wait briefly for the new nodes to register before labeling.
	for _, j := range jobs {
		if err := r.Wait(ctx, "", "Ready", "node/"+j.dpu.Hostname, 5*time.Minute); err != nil {
			return fmt.Errorf("wait node %s: %w", j.dpu.Hostname, err)
		}
	}
	for _, j := range jobs {
		if err := r.Kubectl(ctx, "label", "node", j.dpu.Hostname, "app=f5-tmm", "--overwrite"); err != nil {
			return fmt.Errorf("label %s: %w", j.dpu.Hostname, err)
		}
		if err := r.Kubectl(ctx, "taint", "node", j.dpu.Hostname, "dpu=true:NoSchedule", "--overwrite"); err != nil {
			return fmt.Errorf("taint %s: %w", j.dpu.Hostname, err)
		}
		fmt.Fprintf(out, "      %s labeled+tainted\n", j.dpu.Hostname)
	}
	return nil
}

// redactToken hides the bootstrap token in journal/log output. The CA
// hash is fine to show; the token itself is the bearer credential.
func redactToken(joinCmd string) string {
	idx := strings.Index(joinCmd, "--token ")
	if idx < 0 {
		return joinCmd
	}
	rest := joinCmd[idx+len("--token "):]
	end := strings.IndexByte(rest, ' ')
	if end < 0 {
		return joinCmd[:idx] + "--token <redacted>"
	}
	return joinCmd[:idx] + "--token <redacted>" + rest[end:]
}

// appendJoinJournal records the join outcome. labelTaintErr is nil when the
// DPU nodes were successfully labeled/tainted (BNK-ready); non-nil when the
// join succeeded but label/taint failed — in which case the journal must NOT
// claim the nodes are labeled, and must point at the required retry.
func appendJoinJournal(repo, pocName string, jobs []dpuJob, labelTaintErr error) {
	outcome := "joined cluster"
	if labelTaintErr != nil {
		outcome = "joined cluster but label/taint FAILED"
	}
	header := fmt.Sprintf("join-dpus — %d DPU(s) %s", len(jobs), outcome)
	f, err := openJournal(repo, "cluster", header)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "- PoC:  %s\n", pocName)
	for _, j := range jobs {
		if labelTaintErr == nil {
			fmt.Fprintf(f, "- %s (host %s, dpu %s, tmfifo %s) — labeled app=f5-tmm, tainted dpu=true:NoSchedule\n",
				j.dpu.Hostname, j.host.Name, j.dpu.PCI, j.dpu.TmfifoIP)
		} else {
			fmt.Fprintf(f, "- %s (host %s, dpu %s, tmfifo %s) — joined but NOT labeled/tainted\n",
				j.dpu.Hostname, j.host.Name, j.dpu.PCI, j.dpu.TmfifoIP)
		}
	}
	if labelTaintErr != nil {
		fmt.Fprintf(f, "- label/taint error: %v\n", labelTaintErr)
		fmt.Fprintln(f, "- Node NOT BNK-ready — CNE needs the node labeled app=f5-tmm and tolerating dpu=true:NoSchedule. Re-run `dpubnkctl cluster join-dpus`.")
	} else {
		fmt.Fprintln(f, "- Next: pre-sales SE confirms `kubectl get nodes -o wide` shows N+M nodes Ready")
	}
	fmt.Fprintln(f)
}
