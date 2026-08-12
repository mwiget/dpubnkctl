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
	airgap         string
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
	cmd.Flags().StringVar(&f.airgap, "airgap", "", "Airgap mode (propagated from e2e)")
	return cmd
}

type dpuJob struct {
	host *poc.Host
	dpu  *poc.DPU
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
	fmt.Fprintf(out, "Joining:    %d DPU(s) across %d host(s)\n\n", len(jobs), len(p.Hosts))

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
	// Preference: poc.network.cluster_apiserver_address (the data-plane
	// VIP/IP that all kubelets advertise to). Falls back to the control
	// plane's SSH address — preserves the previous behavior when the
	// PoC has no data-plane network.
	apiserverAddr := p.Network.ClusterAPIServerAddress
	if apiserverAddr == "" {
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
	nodeIPRole := p.Network.NodeIPRole
	for _, j := range jobs {
		j := j
		wg.Add(1)
		go func() {
			defer wg.Done()
			tag := fmt.Sprintf("[%s]", j.dpu.Hostname)
			nodeIP, err := resolveDPUNodeIP(j.dpu, nodeIPRole)
			if err != nil {
				mu.Lock()
				failures = append(failures, fmt.Sprintf("%s: %v", j.dpu.Hostname, err))
				mu.Unlock()
				fmt.Fprintf(out, "%s ERR: %v\n", tag, err)
				return
			}
			outcome, err := joinOneDPU(ctx, repo, j, jc, nodeIP, p.Versions.K8s, f, prefixWriter{w: out, prefix: tag + " "})
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
	if !f.skipLabelTaint && len(joined) > 0 {
		fmt.Fprintf(out, "[3/3] Labeling + tainting %d DPU node(s) ...\n", len(joined))
		if err := labelAndTaintDPUs(ctx, repo, joined, out); err != nil {
			fmt.Fprintf(out, "      WARN: label/taint partial failure: %v\n", err)
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

	// Update poc.yaml + journal.
	p.Status.Cluster = "completed"
	p.Status.LastPhaseAt = time.Now().UTC()
	if err := savePoC(repo, p, out); err != nil {
		return err
	}
	appendJoinJournal(repo, p.Metadata.Name, jobs)
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

// resolveDPUNodeIP picks the kubelet --node-ip for a DPU. When
// network.node_ip_role is set, returns the bare IP from the matching
// dpu.vlans entry; when unset, returns "" so kubeadm/kubelet auto-detect
// (legacy behavior — yields the oob_net0/mgmt IP).
func resolveDPUNodeIP(d *poc.DPU, role string) (string, error) {
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

// joinOutcome distinguishes "did a real kubeadm join" from "DPU was
// already a cluster member, fast-path". The outer caller still treats
// both as success — this only changes the user-facing message and the
// journal entry.
type joinOutcome int

const (
	joinOutcomeJoined        joinOutcome = iota // ran kubeadm join, OK
	joinOutcomeAlreadyJoined                    // /etc/kubernetes/kubelet.conf already present
)

func joinOneDPU(ctx context.Context, repo string, j dpuJob, jc *cluster.JoinCommand, nodeIP, k8sMinor string, f *clusterJoinDPUsFlags, w io.Writer) (joinOutcome, error) {
	// Pre-dial step: make sure the host's tmfifo_net0 has 192.168.100.1/30
	// before we try to reach the DPU at 192.168.100.2. The rshim kernel
	// module *should* assign the host address at module load, but any
	// `systemctl restart rshim` (operator clearing an orphaned console
	// reader, or any other lifecycle event) wipes it. Without the .1/30,
	// the DPU dial times out with "context deadline exceeded" — observed
	// twice on the ailab single-node PoC across `provision dpu` and
	// `cluster join-dpus`. Idempotent — `ip addr add` returns "File exists"
	// (which ensureHostTmfifoIP swallows) when the address is already set.
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
	ensureHostTmfifoIP(ctx, hostC)
	hostC.Close()

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

	if !f.skipInstall {
		if f.airgap != "" {
			pocData, _ := poc.Load(repo)
			jumphostIP := ""
			if pocData != nil && pocData.Airgap != nil {
				jumphostIP = pocData.Airgap.JumphostIP
			}

			debDir := filepath.Join(repo, "artifacts", "airgap", "dpu-debs")
			imgDir := filepath.Join(repo, "artifacts", "airgap", "images-dpu")

			// Push files via PF (data-plane link, ~8x faster than tmfifo)
			dpuPFIP := dpuInternalIP(j.dpu)
			if dpuPFIP == "" {
				return joinOutcomeJoined, fmt.Errorf("no internal VLAN IP found for DPU %s — cannot push files via PF", j.dpu.Hostname)
			}
			hostCfg2, cfgErr := sshConfigForHost(repo, j.host, 30*time.Second)
			if cfgErr != nil {
				return joinOutcomeJoined, fmt.Errorf("host ssh config for PF push: %w", cfgErr)
			}
			dialCtx2, cancel2 := context.WithTimeout(ctx, 30*time.Second)
			hostC2, hostErr := ssh.Dial(dialCtx2, hostCfg2)
			cancel2()
			if hostErr != nil {
				return joinOutcomeJoined, fmt.Errorf("ssh host for PF push: %w", hostErr)
			}
			defer hostC2.Close()

			fmt.Fprintf(w, "pushing files via PF (%s) ...\n", dpuPFIP)
			hostKeyPath := j.host.SSH.KeyRef
			if !filepath.IsAbs(hostKeyPath) {
				hostKeyPath = filepath.Join(repo, hostKeyPath)
			}
			if err := pushViaPF(ctx, w, hostC2, c, debDir, "/tmp/airgap-debs", dpuPFIP, hostKeyPath); err != nil {
				return joinOutcomeJoined, fmt.Errorf("PF push debs failed (data-plane not working): %w", err)
			}
			if err := pushViaPF(ctx, w, hostC2, c, imgDir, "/tmp/airgap-images", dpuPFIP, hostKeyPath); err != nil {
				return joinOutcomeJoined, fmt.Errorf("PF push images failed (data-plane not working): %w", err)
			}

			fmt.Fprintln(w, "installing kubelet/kubeadm/kubectl offline ...")
			instCtx, icancel := context.WithTimeout(ctx, 10*time.Minute)
			apiServerIP := ""
			if pocData != nil {
				apiServerIP = strings.SplitN(pocData.Network.ClusterAPIServerAddress, ":", 2)[0]
			}
			err := cluster.InstallKubeBinariesOffline(instCtx, c, k8sMinor, jumphostIP, apiServerIP)
			icancel()
			if err != nil {
				return joinOutcomeJoined, err
			}

			fmt.Fprintln(w, "importing container images ...")
			if err := cluster.ImportContainerImages(ctx, c, "/tmp/airgap-images", jumphostIP+":5000"); err != nil {
				return joinOutcomeJoined, err
			}
			fmt.Fprintln(w, "airgap install ok")
		} else {
			fmt.Fprintln(w, "installing kubelet/kubeadm/kubectl ...")
			instCtx, icancel := context.WithTimeout(ctx, 10*time.Minute)
			err := cluster.InstallKubeBinaries(instCtx, c, k8sMinor)
			icancel()
			if err != nil {
				return joinOutcomeJoined, err
			}
			fmt.Fprintln(w, "install ok")
		}
	}

	if nodeIP != "" {
		fmt.Fprintf(w, "running kubeadm join (--node-ip %s) ...\n", nodeIP)
	} else {
		fmt.Fprintln(w, "running kubeadm join ...")
	}
	if _, err := cluster.JoinDPU(ctx, c, jc, j.dpu.Hostname, nodeIP); err != nil {
		return joinOutcomeJoined, fmt.Errorf("kubeadm join failed on %s: %w\n\nTo retry, run on the DPU:\n  sudo kubeadm reset -f\n  sudo rm -rf /etc/kubernetes /var/lib/kubelet\nThen re-run this phase.", j.dpu.Hostname, err)
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

func pushDirToRemote(ctx context.Context, c *ssh.Client, localDir, remoteDir string) error {
	c.Run(ctx, "mkdir -p "+remoteDir)
	entries, err := os.ReadDir(localDir)
	if err != nil {
		return fmt.Errorf("read %s: %w", localDir, err)
	}

	if remoteMatchesLocal(ctx, c, entries, remoteDir) {
		return nil
	}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		local := filepath.Join(localDir, e.Name())
		remote := remoteDir + "/" + e.Name()
		if err := c.PushFile(ctx, local, remote, nil); err != nil {
			return fmt.Errorf("push %s: %w", e.Name(), err)
		}
	}
	return nil
}

func remoteMatchesLocal(ctx context.Context, c *ssh.Client, localEntries []os.DirEntry, remoteDir string) bool {
	r := c.Run(ctx, fmt.Sprintf("ls -1 %s 2>/dev/null | wc -l", remoteDir))
	if !r.OK() {
		return false
	}
	remoteCount := strings.TrimSpace(r.Stdout)
	var localCount int
	for _, e := range localEntries {
		if !e.IsDir() {
			localCount++
		}
	}
	if fmt.Sprintf("%d", localCount) != remoteCount {
		return false
	}
	return localCount > 0
}

func dpuInternalIP(dpu *poc.DPU) string {
	for _, v := range dpu.VLANs {
		if v.Role == "internal" {
			return strings.SplitN(v.IP, "/", 2)[0]
		}
	}
	return ""
}

func pushViaPF(ctx context.Context, w io.Writer, hostC *ssh.Client, dpuC *ssh.Client, localDir, remoteDir, dpuPFIP, hostKeyPath string) error {
	entries, err := os.ReadDir(localDir)
	if err != nil {
		return fmt.Errorf("read %s: %w", localDir, err)
	}

	dpuC.Run(ctx, "mkdir -p "+remoteDir)

	if remoteMatchesLocal(ctx, dpuC, entries, remoteDir) {
		fmt.Fprintf(w, "  %s already on DPU — skipping\n", filepath.Base(localDir))
		return nil
	}

	hostTmpDir := "/tmp/airgap-stage-" + filepath.Base(localDir)
	hostC.Run(ctx, "mkdir -p "+hostTmpDir)

	// Push the SSH key to the host so it can SCP to the DPU
	hostTmpKey := "/tmp/airgap-dpu-key"
	if err := hostC.PushFile(ctx, hostKeyPath, hostTmpKey, nil); err != nil {
		return fmt.Errorf("push SSH key to host: %w", err)
	}
	hostC.Run(ctx, "chmod 600 "+hostTmpKey)

	var fileCount int
	fmt.Fprintf(w, "  jumphost → host: %s ...\n", filepath.Base(localDir))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		local := filepath.Join(localDir, e.Name())
		remote := hostTmpDir + "/" + e.Name()
		if err := hostC.PushFile(ctx, local, remote, nil); err != nil {
			return fmt.Errorf("push to host %s: %w", e.Name(), err)
		}
		fileCount++
	}
	fmt.Fprintf(w, "  jumphost → host: %d files done\n", fileCount)

	fmt.Fprintf(w, "  host → DPU via PF: %s ...\n", filepath.Base(localDir))
	scpCmd := fmt.Sprintf("scp -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -i %s %s/* ubuntu@%s:%s/",
		hostTmpKey, hostTmpDir, dpuPFIP, remoteDir)
	r := hostC.Run(ctx, scpCmd)

	// Cleanup: remove key and staging dir from host
	hostC.Run(ctx, "rm -f "+hostTmpKey)
	hostC.Run(ctx, "rm -rf "+hostTmpDir)

	if !r.OK() {
		return fmt.Errorf("scp to DPU: exit=%d stderr=%s", r.ExitCode, r.Stderr)
	}
	return nil
}

func appendJoinJournal(repo, pocName string, jobs []dpuJob) {
	header := fmt.Sprintf("join-dpus — %d DPU(s) joined cluster", len(jobs))
	f, err := openJournal(repo, "cluster", header)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "- PoC:  %s\n", pocName)
	for _, j := range jobs {
		fmt.Fprintf(f, "- %s (host %s, dpu %s, tmfifo %s) — labeled app=f5-tmm, tainted dpu=true:NoSchedule\n",
			j.dpu.Hostname, j.host.Name, j.dpu.PCI, j.dpu.TmfifoIP)
	}
	fmt.Fprintln(f, "- Next: pre-sales SE confirms `kubectl get nodes -o wide` shows N+M nodes Ready")
	fmt.Fprintln(f)
}
