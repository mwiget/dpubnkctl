package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
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

func runClusterJoinDPUs(ctx context.Context, out io.Writer, f *clusterJoinDPUsFlags) error {
	repo, err := resolvePoCDir(f.pocDir)
	if err != nil {
		return err
	}
	p, err := poc.Load(repo)
	if err != nil {
		return fmt.Errorf("not a PoC repo (%s): %w", repo, err)
	}
	if !f.yolo {
		return errors.New("refusing destructive DPU join without --yolo")
	}
	if f.confirmCluster != p.Metadata.Name {
		return fmt.Errorf("--confirm-cluster must equal poc.yaml.metadata.name (%q), got %q", p.Metadata.Name, f.confirmCluster)
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
	// Rewrite 127.0.0.1 → the control plane's routable address so the
	// DPU (which can reach the host but not the host's loopback) joins
	// against the right endpoint.
	jc, err := cluster.FetchJoinCommand(ctx, cpClient, cpHost.SSH.Address)
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
	)
	for _, j := range jobs {
		j := j
		wg.Add(1)
		go func() {
			defer wg.Done()
			tag := fmt.Sprintf("[%s]", j.dpu.Hostname)
			err := joinOneDPU(ctx, repo, j, jc, p.Versions.K8s, f, prefixWriter{w: out, prefix: tag + " "})
			if err != nil {
				mu.Lock()
				failures = append(failures, fmt.Sprintf("%s: %v", j.dpu.Hostname, err))
				mu.Unlock()
				fmt.Fprintf(out, "%s ERR: %v\n", tag, err)
				return
			}
			fmt.Fprintf(out, "%s joined.\n", tag)
		}()
	}
	wg.Wait()

	if len(failures) > 0 {
		return fmt.Errorf("%d DPU join(s) failed:\n  - %s", len(failures), strings.Join(failures, "\n  - "))
	}

	// 3. Label + taint via operator's kubectl.
	if !f.skipLabelTaint {
		fmt.Fprintln(out, "[3/3] Labeling + tainting DPU nodes ...")
		if err := labelAndTaintDPUs(ctx, repo, jobs, out); err != nil {
			return fmt.Errorf("label/taint: %w", err)
		}
	} else {
		fmt.Fprintln(out, "[3/3] (--skip-label-taint)")
	}

	// Update poc.yaml + journal.
	p.Status.Cluster = "completed"
	p.Status.LastPhaseAt = time.Now().UTC()
	if err := p.Save(repo); err != nil {
		return err
	}
	appendJoinJournal(repo, p.Metadata.Name, jobs)
	fmt.Fprintln(out, "\nDONE.")
	return nil
}

func joinOneDPU(ctx context.Context, repo string, j dpuJob, jc *cluster.JoinCommand, k8sMinor string, f *clusterJoinDPUsFlags, w io.Writer) error {
	dpuIP := strings.SplitN(j.dpu.TmfifoIP, "/", 2)[0]

	hostKey := j.host.SSH.KeyRef
	if !filepath.IsAbs(hostKey) {
		hostKey = filepath.Join(repo, hostKey)
	}
	known := filepath.Join(repo, "inventory", "known_hosts")

	// DPU connections deliberately skip known_hosts: every DPU answers
	// at the same tmfifo IP (192.168.100.2) but is a different machine,
	// so a shared known_hosts collides on the second host. The trust
	// boundary is the jumphost (which IS verified via known_hosts).
	cfg := ssh.Config{
		Address: dpuIP,
		Port:    22,
		User:    "ubuntu",
		KeyPath: hostKey,
		Timeout: 30 * time.Second,
		Jumphost: &ssh.Config{
			Address:    j.host.SSH.Address,
			Port:       j.host.SSH.Port,
			User:       j.host.SSH.User,
			KeyPath:    hostKey,
			KnownHosts: known,
			Timeout:    30 * time.Second,
		},
	}

	fmt.Fprintf(w, "ssh ubuntu@%s via %s ...\n", dpuIP, j.host.SSH.Address)
	dialCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	c, err := ssh.Dial(dialCtx, cfg)
	cancel()
	if err != nil {
		return fmt.Errorf("ssh dpu: %w", err)
	}
	defer c.Close()

	if !f.skipInstall {
		fmt.Fprintln(w, "installing kubelet/kubeadm/kubectl ...")
		instCtx, icancel := context.WithTimeout(ctx, 10*time.Minute)
		err := cluster.InstallKubeBinaries(instCtx, c, k8sMinor)
		icancel()
		if err != nil {
			return err
		}
		fmt.Fprintln(w, "install ok")
	}

	fmt.Fprintln(w, "running kubeadm join ...")
	if err := cluster.JoinDPU(ctx, c, jc, j.dpu.Hostname); err != nil {
		return err
	}
	return nil
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

func appendJoinJournal(repo, pocName string, jobs []dpuJob) {
	date := time.Now().UTC().Format("2006-01-02")
	path := filepath.Join(repo, "journal", date+"-cluster.md")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "## lab-tech: join-dpus — %d DPU(s) joined cluster\n", len(jobs))
	fmt.Fprintf(f, "- Time: %s\n", time.Now().UTC().Format(time.RFC3339))
	fmt.Fprintf(f, "- PoC:  %s\n", pocName)
	for _, j := range jobs {
		fmt.Fprintf(f, "- %s (host %s, dpu %s, tmfifo %s) — labeled app=f5-tmm, tainted dpu=true:NoSchedule\n",
			j.dpu.Hostname, j.host.Name, j.dpu.PCI, j.dpu.TmfifoIP)
	}
	fmt.Fprintln(f, "- Next: pre-sales SE confirms `kubectl get nodes -o wide` shows N+M nodes Ready")
	fmt.Fprintln(f)
}
