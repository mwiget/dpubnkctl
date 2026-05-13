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

// newDestroyCmd assembles the destroy command tree:
//
//	dpubnkctl destroy        — bnk → dpus → cluster reset (everything)
//	dpubnkctl destroy bnk    — CNEInstance + FLO + sub-CRs only
//	dpubnkctl destroy dpus   — kubeadm reset on every DPU + delete node objects
//
// All require --yolo and --confirm-cluster matching poc.yaml.metadata.name.
// Cluster reset itself stays under `cluster reset` (already implemented).
func newDestroyCmd() *cobra.Command {
	f := &destroyAllFlags{}
	cmd := &cobra.Command{
		Use:   "destroy",
		Short: "Tear down BNK + DPUs + cluster (DESTRUCTIVE — gated by --yolo)",
		Long: `Tear down everything dpubnkctl deploys, in dependency order:

  [1/3] destroy bnk     — delete CNEInstance(s), force-clean F5 sub-CRs,
                          helm uninstall flo, delete cert-manager + the
                          f5-operators namespace
  [2/3] destroy dpus    — kubeadm reset each DPU in parallel via SSH,
                          delete the corresponding node objects
  [3/3] cluster reset   — kubespray reset.yml against the host nodes

After this the hosts still have the data-plane VLAN sub-interfaces
(safe to leave; harmless on next deploy). DPU OS is left intact.

Required gates:
  --yolo                   acknowledge that this is destructive
  --confirm-cluster NAME   must equal poc.yaml.metadata.name`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDestroyAll(cmd.Context(), cmd.OutOrStdout(), f)
		},
	}
	cmd.Flags().StringVar(&f.pocDir, "poc", "", "PoC repo path (default: current directory)")
	cmd.Flags().BoolVar(&f.yolo, "yolo", false, "Acknowledge destructive cleanup")
	cmd.Flags().StringVar(&f.confirmCluster, "confirm-cluster", "", "Must equal poc.yaml.metadata.name (typo guard)")
	cmd.Flags().DurationVar(&f.timeout, "timeout", 30*time.Minute, "Per-phase wall-clock timeout")
	cmd.Flags().BoolVar(&f.skipBNK, "skip-bnk", false, "Skip BNK teardown (start at DPU reset)")
	cmd.Flags().BoolVar(&f.skipDPUs, "skip-dpus", false, "Skip DPU kubeadm reset")
	cmd.Flags().BoolVar(&f.skipCluster, "skip-cluster", false, "Skip kubespray reset.yml")

	cmd.AddCommand(newDestroyBNKCmd(), newDestroyDPUsCmd())
	return cmd
}

type destroyAllFlags struct {
	pocDir         string
	yolo           bool
	confirmCluster string
	timeout        time.Duration
	skipBNK        bool
	skipDPUs       bool
	skipCluster    bool
}

func runDestroyAll(ctx context.Context, out io.Writer, f *destroyAllFlags) error {
	repo, err := resolvePoCDir(f.pocDir)
	if err != nil {
		return err
	}
	p, err := poc.Load(repo)
	if err != nil {
		return fmt.Errorf("not a PoC repo (%s): %w", repo, err)
	}
	if !f.yolo {
		return errors.New("refusing destructive teardown without --yolo")
	}
	if f.confirmCluster != p.Metadata.Name {
		return fmt.Errorf("--confirm-cluster must equal poc.yaml.metadata.name (%q), got %q", p.Metadata.Name, f.confirmCluster)
	}

	fmt.Fprintf(out, "PoC: %s\n\n", p.Metadata.Name)

	if !f.skipBNK {
		fmt.Fprintln(out, "[1/3] destroy bnk ...")
		if err := destroyBNK(ctx, repo, p, out, f.timeout); err != nil {
			return fmt.Errorf("destroy bnk: %w", err)
		}
		fmt.Fprintln(out, "      bnk torn down.")
	} else {
		fmt.Fprintln(out, "[1/3] (--skip-bnk)")
	}

	if !f.skipDPUs {
		fmt.Fprintln(out, "\n[2/3] destroy dpus ...")
		if err := destroyDPUs(ctx, repo, p, out, f.timeout); err != nil {
			return fmt.Errorf("destroy dpus: %w", err)
		}
		fmt.Fprintln(out, "      dpus reset.")
	} else {
		fmt.Fprintln(out, "[2/3] (--skip-dpus)")
	}

	if !f.skipCluster {
		fmt.Fprintln(out, "\n[3/3] cluster reset (kubespray) ...")
		if err := destroyClusterReset(ctx, repo, p, out, f.timeout); err != nil {
			return fmt.Errorf("cluster reset: %w", err)
		}
		fmt.Fprintln(out, "      cluster reset.")
	} else {
		fmt.Fprintln(out, "[3/3] (--skip-cluster)")
	}

	p.Status.Deploy = "pending"
	p.Status.Cluster = "pending"
	p.Status.LastPhaseAt = time.Now().UTC()
	if err := p.Save(repo); err != nil {
		return err
	}
	appendDestroyJournal(repo, p.Metadata.Name, "ALL", "")
	fmt.Fprintln(out, "\nDONE.  Re-run `dpubnkctl host network setup` + `cluster up` + `cluster join-dpus` + `deploy ...` to redeploy.")
	return nil
}

// ----------------------------------------------------------------------
// destroy bnk
// ----------------------------------------------------------------------

type destroyBNKFlags struct {
	pocDir         string
	yolo           bool
	confirmCluster string
	timeout        time.Duration
}

func newDestroyBNKCmd() *cobra.Command {
	f := &destroyBNKFlags{}
	cmd := &cobra.Command{
		Use:   "bnk",
		Short: "Tear down BNK platform (CNEInstance, FLO, cert-manager, F5 sub-CRs) — leaves cluster intact",
		Long: `Cluster-side cleanup of everything deploy-bnk created:

  - delete CNEInstance(s) cluster-wide and wait for the FLO finalizer
  - force-delete orphan F5 sub-CRs in f5-operators (csrcs, cwcs,
    observers, rabbitmqs, otelcollectors, dssms, etc.) including
    finalizer patching for any stuck on Terminating
  - helm uninstall flo
  - delete cert-manager + the f5-operators namespace
  - delete F5SPKVlan CRs + sf NADs in default

DPU node objects, host kubelet, kubeconfig, kubespray inventory all
left alone — use destroy dpus + cluster reset for those.

Required gates:
  --yolo                   acknowledge cluster writes
  --confirm-cluster NAME   must equal poc.yaml.metadata.name`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDestroyBNK(cmd.Context(), cmd.OutOrStdout(), f)
		},
	}
	cmd.Flags().StringVar(&f.pocDir, "poc", "", "PoC repo path (default: current directory)")
	cmd.Flags().BoolVar(&f.yolo, "yolo", false, "Acknowledge cluster writes")
	cmd.Flags().StringVar(&f.confirmCluster, "confirm-cluster", "", "Must equal poc.yaml.metadata.name (typo guard)")
	cmd.Flags().DurationVar(&f.timeout, "timeout", 15*time.Minute, "FLO finalizer + force-clean wall-clock")
	return cmd
}

func runDestroyBNK(ctx context.Context, out io.Writer, f *destroyBNKFlags) error {
	repo, err := resolvePoCDir(f.pocDir)
	if err != nil {
		return err
	}
	p, err := poc.Load(repo)
	if err != nil {
		return fmt.Errorf("not a PoC repo (%s): %w", repo, err)
	}
	if !f.yolo {
		return errors.New("refusing destructive bnk teardown without --yolo")
	}
	if f.confirmCluster != p.Metadata.Name {
		return fmt.Errorf("--confirm-cluster must equal poc.yaml.metadata.name (%q), got %q", p.Metadata.Name, f.confirmCluster)
	}
	if err := destroyBNK(ctx, repo, p, out, f.timeout); err != nil {
		return err
	}
	appendDestroyJournal(repo, p.Metadata.Name, "BNK", "")
	fmt.Fprintln(out, "\nDONE.")
	return nil
}

// destroyBNK is the workhorse, callable from both `destroy` and `destroy bnk`.
func destroyBNK(ctx context.Context, repo string, p *poc.PoC, out io.Writer, timeout time.Duration) error {
	kubeconfig := filepath.Join(repo, "artifacts", "kubeconfig")
	if _, err := os.Stat(kubeconfig); err != nil {
		return fmt.Errorf("kubeconfig %s missing — nothing to clean up cluster-side", kubeconfig)
	}
	r := &deploy.Runner{KubeconfigPath: kubeconfig, Out: prefixWriter{w: out, prefix: "      | "}}

	// 1. Delete CNEInstances cluster-wide. FLO will start its finalizer
	//    chain. We don't wait for convergence — observed in lake1, FLO
	//    cleanly removes its own CNEInstance finalizer but leaves sub-CR
	//    children (csrcs/cwcs/observers/...) with stuck finalizers, so
	//    the force-clean step below is the real safety net.
	fmt.Fprintln(out, "      → delete CNEInstance(s) cluster-wide")
	_ = r.Kubectl(ctx, "delete", "cneinstance", "--all-namespaces", "--all", "--ignore-not-found", "--wait=false")
	// Brief grace so FLO at least starts its termination work — accelerates
	// the next steps when they're racing it.
	time.Sleep(10 * time.Second)

	// 3. Force-delete F5 sub-CRs in f5-operators (the orphans). List of
	//    plural names matches what FLO's chart installs; missing ones
	//    just no-op via --ignore-not-found.
	subCRs := []string{
		"csrcs", "cwcs", "observers", "rabbitmqs", "otelcollectors",
		"cnemanifests", "crdinstallers", "afms", "downloaders", "dssms",
		"ipams", "envdiscoveries", "dwblds", "coremonds", "analyzers",
		"cnecontrollers",
	}
	fmt.Fprintln(out, "      → force-delete F5 sub-CRs in f5-operators")
	for _, cr := range subCRs {
		_ = r.Kubectl(ctx, "-n", "f5-operators", "delete", cr+".k8s.f5.com", "--all", "--ignore-not-found", "--wait=false", "--timeout=10s")
	}
	// Patch off any finalizers that survive the soft delete.
	fmt.Fprintln(out, "      → strip finalizers from any stuck sub-CRs")
	for _, cr := range subCRs {
		// Scriptable kubectl in one container call: get names → patch each.
		// We use kubectl_patch in a loop driven by `xargs`-style; but our
		// Runner only exposes a single kubectl invocation. Iterate from Go.
		stripFinalizers(ctx, r, "f5-operators", cr+".k8s.f5.com")
	}

	// 4. helm uninstall flo (releases anything the chart created).
	fmt.Fprintln(out, "      → helm uninstall flo")
	_ = r.Helm(ctx, "uninstall", "flo", "--namespace", "f5-operators", "--ignore-not-found", "--wait", "--timeout", timeoutFlag(timeout))

	// 5. Delete the namespaces and stragglers.
	fmt.Fprintln(out, "      → delete f5-operators namespace")
	_ = r.Kubectl(ctx, "delete", "namespace", "f5-operators", "--ignore-not-found", "--wait=false")
	fmt.Fprintln(out, "      → delete cert-manager namespace")
	_ = r.Kubectl(ctx, "delete", "namespace", "cert-manager", "--ignore-not-found", "--wait=false")

	// 6. Default-namespace stragglers — the per-tenant workload that
	//    deploy-cne creates (TMM + dssm + cne-controller etc.) and the
	//    F5SPKVlan/NAD configs we apply manually.
	fmt.Fprintln(out, "      → delete F5SPKVlans + NADs + far-secret in default")
	_ = r.Kubectl(ctx, "-n", "default", "delete", "f5-spk-vlans.k8s.f5net.com", "--all", "--ignore-not-found", "--wait=false")
	_ = r.Kubectl(ctx, "-n", "default", "delete", "net-attach-def", "sf-external", "sf-internal", "--ignore-not-found", "--wait=false")
	_ = r.Kubectl(ctx, "-n", "default", "delete", "secret", "far-secret", "--ignore-not-found", "--wait=false")
	// The CNEInstance-created workload deployments/statefulsets/dameonsets
	// in default usually delete with the CNEInstance, but sweep up any
	// that are still around.
	for _, ws := range []string{"deploy", "statefulset", "daemonset"} {
		_ = r.Kubectl(ctx, "-n", "default", "delete", ws,
			"-l", "app.kubernetes.io/managed-by=f5-lifecycle-operator",
			"--ignore-not-found", "--wait=false")
	}
	return nil
}

// stripFinalizers patches `metadata.finalizers: []` on every CR of the
// given kind in the given namespace. Best-effort; errors are streamed
// to r.Out via the Runner but never fail the overall destroy.
//
// Note: kubectl patch --all --type=merge applies the patch to every
// matching object in one call. If the kind has no instances, the
// --ignore-not-found flag absorbs it.
func stripFinalizers(ctx context.Context, r *deploy.Runner, ns, kind string) {
	_ = r.Kubectl(ctx, "-n", ns, "patch", kind,
		"--type=merge", "-p", `{"metadata":{"finalizers":[]}}`,
		"--all", "--ignore-not-found")
}

// timeoutFlag formats a duration as "5m" for helm's --timeout.
func timeoutFlag(d time.Duration) string {
	if d <= 0 {
		return "5m"
	}
	return d.String()
}

// ----------------------------------------------------------------------
// destroy dpus
// ----------------------------------------------------------------------

type destroyDPUsFlags struct {
	pocDir         string
	yolo           bool
	confirmCluster string
	timeout        time.Duration
}

func newDestroyDPUsCmd() *cobra.Command {
	f := &destroyDPUsFlags{}
	cmd := &cobra.Command{
		Use:   "dpus",
		Short: "kubeadm reset every DPU in poc.yaml + delete the node objects (DESTRUCTIVE on each DPU)",
		Long: `For each DPU in poc.yaml.hosts[].dpus[], in parallel:

  1. SSH to the DPU through its host (jumphost) using the operator's key
  2. systemctl disable --now kubelet
  3. kubeadm reset -f --cri-socket unix:///run/containerd/containerd.sock
  4. wipe /etc/cni/net.d, /var/lib/cni, /var/lib/kubelet, /etc/kubernetes
  5. flush leftover Calico/kube iptables rules

Then from the operator side, delete each DPU's Node object so the
cluster forgets them (a fresh join will register cleanly).

Required gates:
  --yolo                   acknowledge DPU OS writes
  --confirm-cluster NAME   must equal poc.yaml.metadata.name`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDestroyDPUs(cmd.Context(), cmd.OutOrStdout(), f)
		},
	}
	cmd.Flags().StringVar(&f.pocDir, "poc", "", "PoC repo path (default: current directory)")
	cmd.Flags().BoolVar(&f.yolo, "yolo", false, "Acknowledge DPU OS writes")
	cmd.Flags().StringVar(&f.confirmCluster, "confirm-cluster", "", "Must equal poc.yaml.metadata.name (typo guard)")
	cmd.Flags().DurationVar(&f.timeout, "timeout", 5*time.Minute, "Per-DPU SSH+reset wall-clock")
	return cmd
}

func runDestroyDPUs(ctx context.Context, out io.Writer, f *destroyDPUsFlags) error {
	repo, err := resolvePoCDir(f.pocDir)
	if err != nil {
		return err
	}
	p, err := poc.Load(repo)
	if err != nil {
		return fmt.Errorf("not a PoC repo (%s): %w", repo, err)
	}
	if !f.yolo {
		return errors.New("refusing destructive DPU reset without --yolo")
	}
	if f.confirmCluster != p.Metadata.Name {
		return fmt.Errorf("--confirm-cluster must equal poc.yaml.metadata.name (%q), got %q", p.Metadata.Name, f.confirmCluster)
	}
	if err := destroyDPUs(ctx, repo, p, out, f.timeout); err != nil {
		return err
	}
	appendDestroyJournal(repo, p.Metadata.Name, "DPUS", "")
	fmt.Fprintln(out, "\nDONE.")
	return nil
}

// destroyDPUs is the workhorse, callable from both `destroy` and `destroy dpus`.
func destroyDPUs(ctx context.Context, repo string, p *poc.PoC, out io.Writer, perJob time.Duration) error {
	type dpuRef struct {
		host *poc.Host
		dpu  *poc.DPU
	}
	var jobs []dpuRef
	for i := range p.Hosts {
		h := &p.Hosts[i]
		for j := range h.DPUs {
			jobs = append(jobs, dpuRef{host: h, dpu: &h.DPUs[j]})
		}
	}
	if len(jobs) == 0 {
		fmt.Fprintln(out, "      no DPUs in poc.yaml — nothing to reset")
		return nil
	}

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
			err := resetOneDPU(ctx, repo, j.host, j.dpu, perJob, prefixWriter{w: out, prefix: tag + " "})
			if err != nil {
				mu.Lock()
				failures = append(failures, fmt.Sprintf("%s: %v", j.dpu.Hostname, err))
				mu.Unlock()
				fmt.Fprintf(out, "%s ERR: %v\n", tag, err)
				return
			}
			fmt.Fprintf(out, "%s reset.\n", tag)
		}()
	}
	wg.Wait()

	// Operator-side: delete the Node objects so the cluster forgets them.
	// Best-effort — kubeconfig may have been wiped if cluster reset
	// already ran, which is fine.
	kubeconfig := filepath.Join(repo, "artifacts", "kubeconfig")
	if _, err := os.Stat(kubeconfig); err == nil {
		fmt.Fprintln(out, "      → delete DPU Node objects from k8s")
		r := &deploy.Runner{KubeconfigPath: kubeconfig, Out: prefixWriter{w: out, prefix: "      | "}}
		for _, j := range jobs {
			_ = r.Kubectl(ctx, "delete", "node", j.dpu.Hostname, "--ignore-not-found", "--wait=false")
		}
	}

	if len(failures) > 0 {
		return fmt.Errorf("%d DPU reset(s) failed:\n  - %s", len(failures), strings.Join(failures, "\n  - "))
	}
	return nil
}

func resetOneDPU(ctx context.Context, repo string, h *poc.Host, d *poc.DPU, timeout time.Duration, w io.Writer) error {
	if d.TmfifoIP == "" {
		return fmt.Errorf("dpu %s missing tmfifo_ip in poc.yaml", d.Hostname)
	}
	dpuIP := strings.SplitN(d.TmfifoIP, "/", 2)[0]

	hostKey := h.SSH.KeyRef
	if !filepath.IsAbs(hostKey) {
		hostKey = filepath.Join(repo, hostKey)
	}
	known := filepath.Join(repo, "inventory", "known_hosts")

	// Same DPU SSH topology as cluster join-dpus: skip known_hosts on
	// the DPU side (every DPU answers at 192.168.100.2 — collisions),
	// trust the jumphost via known_hosts.
	cfg := ssh.Config{
		Address: dpuIP,
		Port:    22,
		User:    "ubuntu",
		KeyPath: hostKey,
		Timeout: 30 * time.Second,
		Jumphost: &ssh.Config{
			Address:    h.SSH.Address,
			Port:       h.SSH.Port,
			User:       h.SSH.User,
			KeyPath:    hostKey,
			KnownHosts: known,
			Timeout:    30 * time.Second,
		},
	}

	fmt.Fprintf(w, "ssh ubuntu@%s via %s ...\n", dpuIP, h.SSH.Address)
	dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	c, err := ssh.Dial(dialCtx, cfg)
	cancel()
	if err != nil {
		return fmt.Errorf("ssh dpu: %w", err)
	}
	defer c.Close()

	// One compound command — fast, single SSH round-trip.
	script := strings.Join([]string{
		"sudo -n systemctl disable --now kubelet 2>/dev/null || true",
		"sudo -n kubeadm reset -f --cri-socket unix:///run/containerd/containerd.sock 2>&1 | tail -5",
		"sudo -n rm -rf /etc/cni/net.d /var/lib/cni /var/lib/kubelet/* /etc/kubernetes/* /etc/kubernetes/.* 2>/dev/null || true",
		// Strip residual KUBE-* / cali- iptables chains. xtables-nft is
		// the active backend on the BSP — iptables-save | restore works.
		"sudo -n iptables-save 2>/dev/null | grep -v KUBE | grep -v cali | sudo -n iptables-restore 2>/dev/null || true",
		// Drop the kubelet env file written by JoinDPU.
		"sudo -n rm -f /etc/default/kubelet || true",
		"echo OK",
	}, "; ")

	runCtx, rcancel := context.WithTimeout(ctx, timeout)
	defer rcancel()
	r := c.Run(runCtx, script)
	if !r.OK() {
		return fmt.Errorf("dpu reset script: exit=%d %s", r.ExitCode, strings.TrimSpace(r.Stderr+r.Stdout))
	}
	fmt.Fprint(w, "kubeadm reset OK\n")
	return nil
}

// ----------------------------------------------------------------------
// destroy → cluster reset (delegates to existing cluster reset path)
// ----------------------------------------------------------------------

func destroyClusterReset(ctx context.Context, repo string, p *poc.PoC, out io.Writer, timeout time.Duration) error {
	plan := cluster.BuildPlan(p)
	if !plan.Valid() {
		return fmt.Errorf("plan invalid: %s", strings.Join(plan.Errors, "; "))
	}
	if _, err := cluster.CheckContainerRuntime(ctx); err != nil {
		return err
	}
	files, err := cluster.Render(p, plan)
	if err != nil {
		return err
	}
	invDir := filepath.Join(repo, "artifacts", "kubespray-inventory")
	for name, content := range files {
		full := filepath.Join(invDir, name)
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			return err
		}
	}
	keysDir := filepath.Join(invDir, "keys")
	if err := os.MkdirAll(keysDir, 0o700); err != nil {
		return err
	}
	for hostName, h := range plan.HostByName {
		src := h.SSH.KeyRef
		if !filepath.IsAbs(src) {
			src = filepath.Join(repo, src)
		}
		data, err := os.ReadFile(src)
		if err != nil {
			return fmt.Errorf("read ssh key for %s (%s): %w", hostName, src, err)
		}
		if err := os.WriteFile(filepath.Join(keysDir, hostName+".pem"), data, 0o600); err != nil {
			return err
		}
	}
	logPath := filepath.Join(repo, "artifacts", "cluster-reset.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return err
	}
	defer logFile.Close()
	tee := io.MultiWriter(prefixWriter{w: out, prefix: "      | "}, logFile)
	exit, runErr := cluster.RunKubespray(ctx, cluster.RunOptions{
		InventoryDir: invDir,
		Out:          tee,
		Timeout:      timeout,
		Playbook:     "reset.yml",
		ExtraArgs:    []string{"-e", "reset_confirmation=yes"},
	})
	if runErr != nil {
		return fmt.Errorf("kubespray: %w", runErr)
	}
	if exit != 0 {
		return fmt.Errorf("kubespray reset exited %d — see %s", exit, logPath)
	}
	fmt.Fprintf(out, "      reset completed — log at %s\n", logPath)
	return nil
}

// appendDestroyJournal mirrors appendDeployJournal for tear-down events.
func appendDestroyJournal(repo, pocName, scope, errMsg string) {
	date := time.Now().UTC().Format("2006-01-02")
	path := filepath.Join(repo, "journal", date+"-destroy.md")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "## lab-tech: destroy — scope=%s\n", scope)
	fmt.Fprintf(f, "- Time: %s\n", time.Now().UTC().Format(time.RFC3339))
	fmt.Fprintf(f, "- PoC:  %s\n", pocName)
	if errMsg != "" {
		fmt.Fprintf(f, "- Error: %s\n", errMsg)
	}
	fmt.Fprintln(f)
}
