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

	"github.com/mwiget/dpubnkctl/internal/bnkforge"
	"github.com/mwiget/dpubnkctl/internal/cluster"
	"github.com/mwiget/dpubnkctl/internal/poc"
	"github.com/mwiget/dpubnkctl/internal/ssh"
)

type clusterUpFlags struct {
	pocDir         string
	yolo           bool
	confirmCluster string
	pull           bool
	skipFetch      bool
	skipBNKForge   bool
	timeout        time.Duration
	playbook       string
}

func newClusterUpCmd() *cobra.Command {
	f := &clusterUpFlags{}
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Run kubespray cluster.yml against the hosts (DESTRUCTIVE — gated by --yolo)",
		Long: `Drive the full Kubernetes cluster bring-up via kubespray (Docker):

  1. Validate the plan (same as 'cluster plan')
  2. Regenerate the kubespray inventory under artifacts/kubespray-inventory
  3. Verify Docker is up and (optionally) pull the kubespray image
  4. Run cluster.yml with the operator's ~/.ssh mounted read-only
  5. Fetch /etc/kubernetes/admin.conf from the first control plane,
     localize the server URL, save as artifacts/kubeconfig (mode 0600)
  6. Update poc.yaml.status.cluster + journal entry

This typically takes 30–90 minutes for a 2-host PoC. Output streams to
the terminal AND to artifacts/cluster-up.log unprefixed.

Required gates:
  --yolo                   acknowledge that this is destructive
  --confirm-cluster NAME   must equal poc.yaml.metadata.name (typo guard)`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runClusterUp(cmd.Context(), cmd.OutOrStdout(), f)
		},
	}
	cmd.Flags().StringVar(&f.pocDir, "poc", "", "PoC repo path (default: current directory)")
	cmd.Flags().BoolVar(&f.yolo, "yolo", false, "Acknowledge that this command is destructive")
	cmd.Flags().StringVar(&f.confirmCluster, "confirm-cluster", "", "Must equal poc.yaml.metadata.name (typo guard)")
	cmd.Flags().BoolVar(&f.pull, "pull", true, "Run `docker pull` for the kubespray image before cluster.yml")
	cmd.Flags().BoolVar(&f.skipFetch, "skip-fetch-kubeconfig", false, "Don't pull /etc/kubernetes/admin.conf back to artifacts/kubeconfig")
	cmd.Flags().BoolVar(&f.skipBNKForge, "skip-bnk-forge", false, "Skip the optional bnk-forge auto-registration even if bnk_forge.enabled=true")
	cmd.Flags().DurationVar(&f.timeout, "timeout", 90*time.Minute, "Wall-clock timeout for the kubespray run")
	cmd.Flags().StringVar(&f.playbook, "playbook", "cluster.yml", "Playbook to run (use reset.yml for tear-down)")
	return cmd
}

func runClusterUp(ctx context.Context, out io.Writer, f *clusterUpFlags) error {
	repo, err := resolvePoCDir(f.pocDir)
	if err != nil {
		return err
	}
	p, err := poc.Load(repo)
	if err != nil {
		return fmt.Errorf("not a PoC repo (%s): %w", repo, err)
	}

	if err := requireTwoGates(f.yolo, "--confirm-cluster", f.confirmCluster, p.Metadata.Name, "cluster bring-up"); err != nil {
		return err
	}
	if err := enforceValidateForPhase(out, p, repo, poc.PhaseCluster, false); err != nil {
		return err
	}

	plan := cluster.BuildPlan(p)
	if !plan.Valid() {
		fmt.Fprintln(out, "=== plan errors ===")
		for _, e := range plan.Errors {
			fmt.Fprintln(out, "  ✗", e)
		}
		return fmt.Errorf("plan is not valid")
	}

	fmt.Fprintf(out, "PoC:        %s   (BNK %s, k8s %s, kubespray %s)\n",
		p.Metadata.Name, p.Metadata.BNKVersion, "v"+p.Versions.K8s, cluster.KubesprayPinForCLI())
	fmt.Fprintf(out, "Plan:       cp=%v  node=%v  etcd=%v\n\n", plan.ControlPlane, plan.Workers, plan.Etcd)

	// 1. Container-runtime preflight (docker or podman).
	fmt.Fprintln(out, "[1/6] Container-runtime preflight ...")
	if _, err := cluster.CheckContainerRuntime(ctx); err != nil {
		return err
	}
	fmt.Fprintln(out, "      ok")

	// 1b. Host VLAN IP preflight. kubespray's "Stop if ip var does not
	//     match local ips" Ansible task fires ~20 min into the play
	//     with zero indication of which host or which IP is the
	//     problem. Pre-check here: for each host, the IP we'll write
	//     into the inventory's `ip:` (== the host's
	//     data_plane.vlans[role==node_ip_role] IP) must actually be
	//     live in `ip -4 addr`. If not, the operator forgot — or
	//     `host network setup` failed — and we tell them so up-front.
	if p.Network.NodeIPRole != "" {
		fmt.Fprintln(out, "      Verifying VLAN IPs are live on every host ...")
		if err := preflightVLANIPs(ctx, repo, plan, p.Network.NodeIPRole, out); err != nil {
			return err
		}
	}

	// 2. Regenerate inventory + stage SSH keys into the inventory tree
	//    (kubespray container reads them from /inventory/keys/<host>.pem).
	fmt.Fprintln(out, "[2/6] Regenerating kubespray inventory ...")
	invDir, err := cluster.StageInventory(repo, p, plan)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "      staged inventory + %d ssh keys under %s\n", len(plan.HostByName), invDir)

	// 3. Pull kubespray image.
	if f.pull {
		fmt.Fprintln(out, "[3/6] Pulling kubespray image ...")
		if err := cluster.PullKubespray(ctx, prefixWriter{w: out, prefix: "      | "}); err != nil {
			return fmt.Errorf("docker pull: %w", err)
		}
		fmt.Fprintln(out, "      ok")
	} else {
		fmt.Fprintln(out, "[3/6] (--pull=false; skipping image pull)")
	}

	// 3.5 Pre-create /etc/kubernetes on every host. Kubespray's `download`
	// role templates kubeadm-images.yaml into kube_config_dir before the
	// preinstall role gets a chance to create it, and `cluster reset`
	// wipes the dir entirely. Pre-create it ourselves to dodge the race.
	fmt.Fprintln(out, "      Pre-creating /etc/kubernetes on every host ...")
	if err := preCreateKubeDir(ctx, repo, plan); err != nil {
		return fmt.Errorf("pre-create /etc/kubernetes: %w", err)
	}

	// 4. Run cluster.yml. Stream to terminal + log file.
	fmt.Fprintf(out, "[4/6] Running kubespray %s (this typically takes 30–90 minutes) ...\n", f.playbook)
	logPath := filepath.Join(repo, "artifacts", "cluster-up.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return err
	}
	defer logFile.Close()

	tee := io.MultiWriter(prefixWriter{w: out, prefix: "      | "}, logFile)

	exit, runErr := cluster.RunKubespray(ctx, cluster.RunOptions{
		InventoryDir: invDir,
		Out:          tee,
		Timeout:      f.timeout,
		Playbook:     f.playbook,
	})
	if runErr != nil {
		appendClusterJournal(repo, p.Metadata.Name, "FAILED (transport)", logPath, runErr.Error())
		return fmt.Errorf("kubespray: %w", runErr)
	}
	if exit != 0 {
		appendClusterJournal(repo, p.Metadata.Name, fmt.Sprintf("FAILED (exit %d)", exit), logPath, "")
		return fmt.Errorf("kubespray exited %d — see %s", exit, logPath)
	}
	fmt.Fprintf(out, "      kubespray completed — log at %s\n", logPath)

	// 5. Restart containerd on every host. Without this, nodes can stay
	//    NotReady because containerd's CRI cached "no CNI" before the
	//    Calico DaemonSet pod wrote /etc/cni/net.d/10-calico.conflist.
	//    Idempotent + cheap — see AGENTS.md #5.
	fmt.Fprintln(out, "[5/6] Restarting containerd on every host ...")
	if err := restartContainerdOnHosts(ctx, repo, plan, out); err != nil {
		fmt.Fprintf(out, "      WARN: containerd restart failed (nodes may stay NotReady — restart manually): %v\n", err)
	}

	// 6. Fetch + localize kubeconfig from first control plane.
	if f.skipFetch {
		fmt.Fprintln(out, "[6/6] (--skip-fetch-kubeconfig)")
	} else {
		fmt.Fprintln(out, "[6/6] Fetching kubeconfig from first control plane ...")
		cpName := plan.ControlPlane[0]
		cpHost := plan.HostByName[cpName]
		kcPath := filepath.Join(repo, "artifacts", "kubeconfig")
		// When cluster_apiserver_address is set in poc.yaml, the inventory
		// adds every host's SSH address to supplementary_addresses_in_ssl_keys
		// so the apiserver cert SAN covers mgmt. In that case we can keep
		// CA verification on. Otherwise fall back to insecure mode — the
		// cert SAN won't include the mgmt address and TLS will fail.
		insecure := p.Network.ClusterAPIServerAddress == ""
		if err := fetchKubeconfig(ctx, repo, cpHost, kcPath, insecure); err != nil {
			fmt.Fprintf(out, "      WARN: kubeconfig fetch failed (cluster is up, fetch manually): %v\n", err)
		} else {
			fmt.Fprintf(out, "      saved %s (kubectl --kubeconfig=%s get nodes)\n", kcPath, kcPath)
		}
	}

	// 7. Update poc.yaml + journal.
	p.Status.Cluster = "completed"
	p.Status.LastPhaseAt = time.Now().UTC()
	if err := savePoC(repo, p, out); err != nil {
		return err
	}
	appendClusterJournal(repo, p.Metadata.Name, "SUCCESS", logPath, "")

	// 8. (Optional) Register the cluster with bnk-forge so the operator
	//    can watch the rest of the deployment live in the UI.
	//
	//    Soft-fail policy (dpubnkctl never installs bnk-forge):
	//      - bnk_forge.enabled=false OR --skip-bnk-forge → skip silently
	//      - stack not running → info-level skip (operator already
	//        decided not to run it; ErrNotRunning)
	//      - any other error (bad creds, API failure) → WARN
	//      Either way cluster-up still succeeds.
	switch {
	case f.skipBNKForge:
		fmt.Fprintln(out, "\n[bnk-forge] --skip-bnk-forge set — not registering with bnk-forge.")
	case !p.BNKForge.Enabled:
		// silent: user has not opted in
	default:
		fmt.Fprintln(out, "\n[bnk-forge] bnk_forge.enabled=true — registering cluster ...")
		err := LaunchBNKForge(ctx, out, repo, p)
		switch {
		case err == nil:
			// success — message already printed by LaunchBNKForge
		case errors.Is(err, bnkforge.ErrNotRunning):
			fmt.Fprintln(out, "[bnk-forge] bnk-forge is not running — skipping. Start it manually and run `dpubnkctl bnk-forge launch` to register.")
		default:
			fmt.Fprintf(out, "[bnk-forge] WARN: registration failed: %v\n", err)
			fmt.Fprintln(out, "[bnk-forge] Continuing — run `dpubnkctl bnk-forge launch` later to retry.")
		}
	}

	fmt.Fprintln(out, "\nDONE.")
	return nil
}

// preCreateKubeDir SSHes to every host (in parallel) and ensures
// /etc/kubernetes exists. Aggregates errors.
func preCreateKubeDir(ctx context.Context, repo string, plan cluster.Plan) error {
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		failures []string
	)
	for name, h := range plan.HostByName {
		name, h := name, h
		wg.Add(1)
		go func() {
			defer wg.Done()
			cfg, err := sshConfigForHost(repo, h, 15*time.Second)
			if err != nil {
				mu.Lock()
				failures = append(failures, fmt.Sprintf("%s: %v", name, err))
				mu.Unlock()
				return
			}
			dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			c, err := ssh.Dial(dialCtx, cfg)
			cancel()
			if err != nil {
				mu.Lock()
				failures = append(failures, fmt.Sprintf("%s: ssh dial: %v", name, err))
				mu.Unlock()
				return
			}
			defer c.Close()
			if r := c.Run(ctx, "sudo -n mkdir -p /etc/kubernetes"); !r.OK() {
				mu.Lock()
				failures = append(failures, fmt.Sprintf("%s: mkdir: %s", name, strings.TrimSpace(r.Stderr+r.Stdout)))
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(failures) > 0 {
		return fmt.Errorf("%d host(s) failed: %s", len(failures), strings.Join(failures, "; "))
	}
	return nil
}

func fetchKubeconfig(ctx context.Context, repo string, host *poc.Host, dst string, insecure bool) error {
	cfg, err := sshConfigForHost(repo, host, 30*time.Second)
	if err != nil {
		return err
	}
	dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c, err := ssh.Dial(dialCtx, cfg)
	if err != nil {
		return fmt.Errorf("ssh dial %s: %w", host.SSH.Address, err)
	}
	defer c.Close()
	r := c.Run(ctx, "sudo -n cat /etc/kubernetes/admin.conf")
	if !r.OK() {
		return fmt.Errorf("read admin.conf: exit=%d stderr=%s", r.ExitCode, strings.TrimSpace(r.Stderr))
	}
	localized := cluster.LocalizeKubeconfig(r.Stdout, host.SSH.Address, insecure)
	return cluster.SaveKubeconfig(dst, localized)
}

// restartContainerdOnHosts SSHes to every host in the plan and runs
// `systemctl restart containerd`. We do this at the end of cluster up
// because containerd's CRI plugin caches its "no CNI" state when the
// plugin starts before /etc/cni/net.d is populated. Calico writes its
// conflist via DaemonSet pod and may race with kubelet's first poll —
// containerd then sits in NetworkPluginNotReady forever until restarted.
// (Reproduced on lake1 worker2 — restart flips the node to Ready in
// seconds.) Idempotent + cheap.
func restartContainerdOnHosts(ctx context.Context, repo string, plan cluster.Plan, out io.Writer) error {
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		failures []string
	)
	for name, h := range plan.HostByName {
		name, h := name, h
		wg.Add(1)
		go func() {
			defer wg.Done()
			cfg, err := sshConfigForHost(repo, h, 15*time.Second)
			if err != nil {
				mu.Lock()
				failures = append(failures, fmt.Sprintf("%s: ssh cfg: %v", name, err))
				mu.Unlock()
				return
			}
			dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			c, err := ssh.Dial(dialCtx, cfg)
			cancel()
			if err != nil {
				mu.Lock()
				failures = append(failures, fmt.Sprintf("%s: ssh dial: %v", name, err))
				mu.Unlock()
				return
			}
			defer c.Close()
			if r := c.Run(ctx, "sudo -n systemctl restart containerd"); !r.OK() {
				mu.Lock()
				failures = append(failures, fmt.Sprintf("%s: restart containerd: %s", name, strings.TrimSpace(r.Stderr+r.Stdout)))
				mu.Unlock()
				return
			}
			fmt.Fprintf(out, "      | %s containerd restarted\n", name)
		}()
	}
	wg.Wait()
	if len(failures) > 0 {
		return fmt.Errorf("%d host(s) failed containerd restart: %s", len(failures), strings.Join(failures, "; "))
	}
	return nil
}

// preflightVLANIPs SSHes every host in the plan and asserts the IP we'll
// pass kubespray as the per-host `ip:` actually appears in the host's
// `ip -4 addr show`. Catches the post-`host network setup`-skipped (or
// failed) cases that otherwise blow up 20 min into kubespray's play
// with an Ansible "Stop if ip var does not match local ips" error.
//
// role is poc.Network.NodeIPRole. Caller guarantees it's non-empty.
func preflightVLANIPs(ctx context.Context, repo string, plan cluster.Plan, role string, out io.Writer) error {
	var failures []string
	for name, h := range plan.HostByName {
		v := h.VLANByRole(role)
		if v == nil {
			failures = append(failures, fmt.Sprintf("%s: no data_plane.vlans[role=%q] entry — set host.data_plane.vlans in poc.yaml", name, role))
			continue
		}
		want := stripCIDR(v.IP)
		cfg, err := sshConfigForHost(repo, h, 15*time.Second)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: ssh config: %v", name, err))
			continue
		}
		dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		c, err := ssh.Dial(dialCtx, cfg)
		cancel()
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: ssh dial: %v", name, err))
			continue
		}
		// inet <ip>/<prefix> matches on any interface. Single grep -q.
		r := c.Run(ctx, fmt.Sprintf("ip -4 addr show | grep -qE 'inet %s/'", want))
		c.Close()
		if !r.OK() {
			failures = append(failures, fmt.Sprintf("%s: IP %s (role=%s) not present on any interface", name, want, role))
			continue
		}
		fmt.Fprintf(out, "      | %s %s OK\n", name, want)
	}
	if len(failures) > 0 {
		return fmt.Errorf("VLAN IP preflight failed for %d host(s) — run `dpubnkctl host network setup` first:\n  - %s",
			len(failures), strings.Join(failures, "\n  - "))
	}
	return nil
}

func appendClusterJournal(repo, pocName, status, logPath, errMsg string) {
	f, err := openJournal(repo, "cluster", "cluster up — "+status)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "- PoC:  %s\n", pocName)
	fmt.Fprintf(f, "- kubespray log: %s\n", strings.TrimPrefix(logPath, repo+string(filepath.Separator)))
	if errMsg != "" {
		fmt.Fprintf(f, "- Error: %s\n", errMsg)
	}
	fmt.Fprintln(f, "- Next: pre-sales SE confirms `kubectl --kubeconfig=artifacts/kubeconfig get nodes` returns Ready")
	fmt.Fprintln(f)
}
