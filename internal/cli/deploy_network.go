package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/mwiget/dpubnkctl/internal/airgap"
	"github.com/mwiget/dpubnkctl/internal/cluster"
	"github.com/mwiget/dpubnkctl/internal/deploy"
	"github.com/mwiget/dpubnkctl/internal/embedded"
	"github.com/mwiget/dpubnkctl/internal/poc"
	"github.com/mwiget/dpubnkctl/internal/ssh"
	"github.com/mwiget/dpubnkctl/internal/version"
)

type deployNetworkFlags struct {
	pocDir        string
	yolo          bool
	confirmDeploy string
	skipPull      bool
	waitTimeout   time.Duration
	airgap        string
}

func newDeployNetworkCmd() *cobra.Command {
	f := &deployNetworkFlags{}
	cmd := &cobra.Command{
		Use:   "network",
		Short: "Install Multus CNI + SR-IOV plugin + NetworkAttachmentDefinitions for BNK (DESTRUCTIVE)",
		Long: `Phase 4c — install the data-plane network plumbing FLO needs:

  multus.yaml                  Multus CNI v4.0.1 (provides the
                               NetworkAttachmentDefinition CRD)
  cni-plugins.yaml             standard CNI plugins (host-device etc.)
  sriovdp-config.yaml          SR-IOV device plugin config:
                               nvidia.com/bf3_p0_sf1, nvidia.com/bf3_p1_sf1
  sriov-cni-daemonset.yaml     SR-IOV CNI daemonset
  sriovdp-daemonset.yaml       SR-IOV device plugin daemonset
                               (registers SF resources to kubelet)
  nad-sf.yaml                  sf-external + sf-internal NADs
                               (type: sf, references nvidia.com/bf3_p*_sf1)

All six manifests are embedded verbatim from
f5-bnk-nvidia-bf3-installations v2.2.0-static/resources/. After this,
FLO controller should stop crashlooping on the missing NAD CRD and
reconcile the CNEInstance applied by 'deploy cne'.

Required gates:
  --yolo                    acknowledge cluster writes
  --confirm-deploy NAME     must equal poc.yaml.metadata.name`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDeployNetwork(cmd.Context(), cmd.OutOrStdout(), f)
		},
	}
	cmd.Flags().StringVar(&f.pocDir, "poc", "", "PoC repo path (default: current directory)")
	cmd.Flags().BoolVar(&f.yolo, "yolo", false, "Acknowledge cluster writes")
	cmd.Flags().StringVar(&f.confirmDeploy, "confirm-deploy", "", "Must equal poc.yaml.metadata.name (typo guard)")
	cmd.Flags().BoolVar(&f.skipPull, "skip-pull", false, "Skip docker pull of alpine/k8s image")
	cmd.Flags().DurationVar(&f.waitTimeout, "wait-timeout", 5*time.Minute, "Per-rollout wait")
	cmd.Flags().StringVar(&f.airgap, "airgap", "", "Airgap mode (propagated from e2e)")
	return cmd
}

// applyOrder is the proven offline-guide sequence: multus thick first
// (provides the NAD CRD + installs CNI binaries), then SR-IOV device
// plugin + config, then NADs. cni-plugins.yaml and sriov-cni-daemonset.yaml
// are NOT used — the thick multus daemon and the sf CNI binary handle
// everything the offline guide needs.
var applyOrder = []string{
	"multus.yaml",
	"sriovdp-config.yaml",
	"sriovdp-daemonset.yaml",
	"nad-sf.yaml",
}

func runDeployNetwork(ctx context.Context, out io.Writer, f *deployNetworkFlags) error {
	repo, err := resolvePoCDir(f.pocDir)
	if err != nil {
		return err
	}
	p, err := poc.Load(repo)
	if err != nil {
		return fmt.Errorf("not a PoC repo (%s): %w", repo, err)
	}
	if err := requireTwoGates(f.yolo, "--confirm-deploy", f.confirmDeploy, p.Metadata.Name, "deploy network"); err != nil {
		return err
	}
	if err := enforceValidateForPhase(out, p, repo, poc.PhaseDeploy, false); err != nil {
		return err
	}
	kubeconfig, err := requireKubeconfig(repo, "run `dpubnkctl cluster up` first")
	if err != nil {
		return err
	}

	r := &deploy.Runner{KubeconfigPath: kubeconfig, Out: prefixWriter{w: out, prefix: "      | "}}

	fmt.Fprintf(out, "PoC:        %s\n", p.Metadata.Name)
	fmt.Fprintf(out, "Cluster:    %s\n\n", kubeconfig)

	if !f.skipPull {
		fmt.Fprintln(out, "[1/5] Tools preflight ...")
		if err := r.CheckTools(ctx); err != nil {
			return err
		}
	}
	fmt.Fprintln(out, "      ok")

	// Save manifests to artifacts/network-rendered/ for SE review.
	stagedDir := filepath.Join(repo, "artifacts", "network-rendered")
	if err := os.MkdirAll(stagedDir, 0o755); err != nil {
		return err
	}

	// ── Step 2: Calico via tigera-operator ──────────────────────────
	// Deploy calico BEFORE multus so the CNI delegate chain is ready
	// when multus starts — eliminates the first-start race where multus
	// records loopback-only delegates in 00-multus.conf.
	fmt.Fprintln(out, "[2/5] Deploying Calico via tigera-operator ...")

	fmt.Fprintln(out, "      → tigera-operator.yaml")
	if err := applyTigeraOperator(ctx, r, repo, p, stagedDir); err != nil {
		return fmt.Errorf("apply tigera-operator: %w", err)
	}

	// Wait for the tigera-operator deployment to be ready.
	fmt.Fprintln(out, "      waiting for tigera-operator ...")
	if err := r.Kubectl(ctx, "rollout", "status", "-n", "tigera-operator",
		"deploy/tigera-operator", "--timeout=3m"); err != nil {
		fmt.Fprintf(out, "      WARN: tigera-operator not ready: %v\n", err)
	}

	// Deploy calico custom-resources (Installation + APIServer CRs).
	calicoRes, err := deploy.RenderCalicoCustomResources(p)
	if err != nil {
		return fmt.Errorf("render calico-custom-resources: %w", err)
	}
	if err := os.WriteFile(filepath.Join(stagedDir, "calico-custom-resources.yaml"), []byte(calicoRes), 0o644); err != nil {
		return err
	}
	fmt.Fprintln(out, "      → calico-custom-resources.yaml")
	if err := r.Apply(ctx, calicoRes); err != nil {
		return fmt.Errorf("apply calico-custom-resources: %w", err)
	}

	// Wait for calico-node DaemonSet to roll out.
	fmt.Fprintln(out, "      waiting for calico-node ...")
	if err := r.Kubectl(ctx, "rollout", "status", "-n", "calico-system",
		"ds/calico-node", "--timeout=5m"); err != nil {
		fmt.Fprintf(out, "      WARN: calico-node not fully Ready: %v\n", err)
	}
	fmt.Fprintln(out, "      calico ready.")

	// coredns and dns-autoscaler are created by kubespray before calico
	// exists. They get stuck because there's no CNI. Delete them now
	// so Kubernetes recreates them with working calico networking.
	fmt.Fprintln(out, "      Restarting coredns + dns-autoscaler ...")
	_ = r.Kubectl(ctx, "-n", "kube-system", "delete", "pod", "-l", "k8s-app=kube-dns", "--ignore-not-found", "--wait=false")
	_ = r.Kubectl(ctx, "-n", "kube-system", "delete", "pod", "-l", "k8s-app=dns-autoscaler", "--ignore-not-found", "--wait=false")

	// ── Step 3: Multus + SR-IOV manifests ───────────────────────────
	fmt.Fprintf(out, "[3/5] Applying %d manifests in order ...\n", len(applyOrder))
	for _, name := range applyOrder {
		raw, err := embedded.Templates.ReadFile("templates/network/" + name)
		if err != nil {
			return fmt.Errorf("read embedded %s: %w", name, err)
		}
		if f.airgap != "" {
			raw = bytes.ReplaceAll(raw, []byte("imagePullPolicy: Always"), []byte("imagePullPolicy: IfNotPresent"))
		}
		if err := os.WriteFile(filepath.Join(stagedDir, name), raw, 0o644); err != nil {
			return err
		}
		fmt.Fprintf(out, "      → %s\n", name)
		for _, ns := range extractNamespaces(raw) {
			if ns == "" || ns == "default" || ns == "kube-system" {
				continue
			}
			if err := r.Apply(ctx, deploy.RenderNamespace(ns)); err != nil {
				return fmt.Errorf("ensure namespace %s for %s: %w", ns, name, err)
			}
		}
		if err := r.Apply(ctx, string(raw)); err != nil {
			return fmt.Errorf("apply %s: %w", name, err)
		}
		if name == "multus.yaml" {
			if err := r.Wait(ctx, "", "Established",
				"crd/network-attachment-definitions.k8s.cni.cncf.io", 2*time.Minute); err != nil {
				fmt.Fprintf(out, "      WARN: NAD CRD not Established: %v\n", err)
			}
		}
	}
	fmt.Fprintln(out, "      all applied.")

	// Restart multus to force a CNI config rescan. The thick multus
	// daemon caches /etc/cni/net.d/ at startup; if it started before
	// calico's install-cni init container dropped the calico conflist
	// on the DPU, multus uses 99-loopback.conf as the cluster network
	// delegate and all pods on that node fail with "missing network name".
	// Restarting after calico is confirmed 1/1 guarantees the rescan
	// picks up the calico config.
	fmt.Fprintln(out, "\n      Restarting multus to pick up calico CNI config ...")
	if err := r.Kubectl(ctx, "rollout", "restart", "-n", "kube-system", "ds/kube-multus-ds"); err != nil {
		fmt.Fprintf(out, "      WARN: multus restart failed: %v\n", err)
	}
	if err := r.Kubectl(ctx, "rollout", "status", "-n", "kube-system", "ds/kube-multus-ds", "--timeout=3m"); err != nil {
		fmt.Fprintf(out, "      WARN: multus rollout not converged: %v\n", err)
	}

	// Delete Pending pods (coredns, dns-autoscaler) that were created
	// before calico was ready — they stay stuck and don't recover.
	fmt.Fprintln(out, "\n      Cleaning up Pending pods ...")
	stuck, _ := r.KubectlCapture(ctx, "get", "pods", "-A",
		"--field-selector=status.phase=Pending",
		"-o", "jsonpath={range .items[*]}{.metadata.namespace}{\" \"}{.metadata.name}{\"\\n\"}{end}")
	for _, line := range strings.Split(strings.TrimSpace(stuck), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		fmt.Fprintf(out, "      deleting %s/%s ...\n", f[0], f[1])
		_ = r.Kubectl(ctx, "-n", f[0], "delete", "pod", f[1], "--ignore-not-found", "--wait=false")
	}

	// ── Step 4: NFS CSI driver + StorageClass ────────────────────────
	fmt.Fprintln(out, "[4/5] Installing NFS CSI driver + StorageClass ...")
	if err := installNFSCSIDriver(ctx, r, repo, p, out); err != nil {
		return fmt.Errorf("NFS CSI driver: %w", err)
	}
	nfsSC, err := deploy.RenderNFSStorageClass(p)
	if err != nil {
		return fmt.Errorf("render nfs-storageclass: %w", err)
	}
	if err := os.WriteFile(filepath.Join(stagedDir, "nfs-storageclass.yaml"), []byte(nfsSC), 0o644); err != nil {
		return err
	}
	if err := r.Apply(ctx, nfsSC); err != nil {
		return fmt.Errorf("apply nfs-storageclass: %w", err)
	}
	fmt.Fprintln(out, "      NFS StorageClass created.")

	// ── Step 5: Wait for DaemonSets ─────────────────────────────────
	fmt.Fprintln(out, "[5/5] Waiting for DaemonSets to be Ready ...")
	for _, ds := range networkDaemonSets {
		fmt.Fprintf(out, "      %s/%s ...\n", ds.namespace, ds.name)
		if err := r.Kubectl(ctx, "rollout", "status",
			"-n", ds.namespace,
			"ds/"+ds.name, "--timeout=3m"); err != nil {
			fmt.Fprintf(out, "      WARN: %s/%s not fully Ready: %v\n", ds.namespace, ds.name, err)
		}
	}

	// Restart containerd on every node so CRI re-scans /etc/cni/net.d.
	fmt.Fprintln(out, "\nRestarting containerd on every node (CRI re-scan) ...")
	if err := restartContainerdEverywhere(ctx, repo, p, out); err != nil {
		fmt.Fprintf(out, "      WARN: containerd restart had errors (some nodes may stay NotReady): %v\n", err)
	}
	fmt.Fprintln(out, "\nWaiting for DaemonSets to converge after containerd restart ...")
	for _, ds := range networkDaemonSets {
		fmt.Fprintf(out, "      %s/%s ...\n", ds.namespace, ds.name)
		if err := r.Kubectl(ctx, "rollout", "status",
			"-n", ds.namespace,
			"ds/"+ds.name, "--timeout=3m"); err != nil {
			fmt.Fprintf(out, "      WARN: %s/%s not fully Ready: %v\n", ds.namespace, ds.name, err)
		}
	}

	appendDeployJournal(repo, p.Metadata.Name, "", "NETWORK INSTALLED", "")
	fmt.Fprintln(out, "\nDONE.  Calico + Multus + SR-IOV + NFS deployed. Ready for `deploy flo`.")
	return nil
}

// installNFSCSIDriver installs the NFS CSI driver via helm. In airgap
// mode it uses the pre-downloaded chart tarball; online, it pulls from
// the upstream chart repo.
func installNFSCSIDriver(ctx context.Context, r *deploy.Runner, repo string, p *poc.PoC, out io.Writer) error {
	nfsValues := fmt.Sprintf(`controller:
  replicas: 1
`)
	if p.Airgap != nil && p.Airgap.Mode != "" {
		chartPattern := filepath.Join(repo, airgap.StagingDir, airgap.ChartsSubDir, version.NFSCSIChartName+"-*.tgz")
		matches, _ := filepath.Glob(chartPattern)
		if len(matches) == 0 {
			return fmt.Errorf("NFS CSI chart not found at %s — run `dpubnkctl airgap setup` first", chartPattern)
		}
		fmt.Fprintf(out, "      helm install (local) %s ...\n", filepath.Base(matches[0]))
		return r.HelmUpgradeLocal(ctx, "csi-driver-nfs", matches[0], "kube-system", nfsValues)
	}
	fmt.Fprintln(out, "      helm install (online) csi-driver-nfs ...")
	return r.HelmUpgrade(ctx, "csi-driver-nfs", version.NFSCSIChartName,
		version.NFSCSIChartRepo, "kube-system", version.NFSCSIDriverVersion, nfsValues)
}

// applyTigeraOperator deploys the tigera-operator manifest. In airgap
// mode it reads the pre-downloaded file; online, kubectl fetches from
// GitHub directly (Docker container has --network=host).
func applyTigeraOperator(ctx context.Context, r *deploy.Runner, repo string, p *poc.PoC, stagedDir string) error {
	if p.Airgap != nil && p.Airgap.Mode != "" {
		path := filepath.Join(repo, airgap.StagingDir, "manifests", "tigera-operator.yaml")
		raw, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read airgap tigera-operator from %s: %w", path, err)
		}
		_ = os.WriteFile(filepath.Join(stagedDir, "tigera-operator.yaml"), raw, 0o644)
		return r.ApplyServerSide(ctx, string(raw))
	}
	return r.Kubectl(ctx, "apply", "--server-side", "--force-conflicts", "-f", version.TigeraOperatorManifest)
}

var networkDaemonSets = []struct {
	namespace string
	name      string
}{
	{"calico-system", "calico-node"},
	{"kube-system", "kube-multus-ds"},
	{"kube-system", "kube-sriov-device-plugin"},
}

// restartContainerdEverywhere SSHes to every host AND every DPU in
// poc.yaml (parallel) and restarts containerd. Safe to no-op on hosts
// where it just bounces a healthy daemon — the cost is ~2s.
func restartContainerdEverywhere(ctx context.Context, repo string, p *poc.PoC, out io.Writer) error {
	type job struct {
		label string
		cfg   ssh.Config
	}
	var jobs []job
	for i := range p.Hosts {
		h := &p.Hosts[i]
		cfg, err := sshConfigForHost(repo, h, 15*time.Second)
		if err != nil {
			fmt.Fprintf(out, "      WARN: skip %s (ssh cfg: %v)\n", h.Name, err)
			continue
		}
		jobs = append(jobs, job{label: h.Name, cfg: cfg})
		// DPUs reached via ProxyJump through their host.
		for j := range h.DPUs {
			d := &h.DPUs[j]
			if d.TmfifoIP == "" || d.Hostname == "" {
				continue
			}
			dpuCfg, err := dpuSSHConfig(repo, h, d)
			if err != nil {
				fmt.Fprintf(out, "      WARN: skip dpu %s (ssh cfg: %v)\n", d.Hostname, err)
				continue
			}
			jobs = append(jobs, job{label: d.Hostname, cfg: dpuCfg})
		}
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
			dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			c, err := ssh.Dial(dialCtx, j.cfg)
			cancel()
			if err != nil {
				mu.Lock()
				failures = append(failures, fmt.Sprintf("%s: ssh dial: %v", j.label, err))
				mu.Unlock()
				return
			}
			defer c.Close()
			if err := cluster.RestartContainerd(ctx, c); err != nil {
				mu.Lock()
				failures = append(failures, fmt.Sprintf("%s: %v", j.label, err))
				mu.Unlock()
				return
			}
			fmt.Fprintf(out, "      | %s containerd restarted\n", j.label)
		}()
	}
	wg.Wait()
	if len(failures) > 0 {
		return fmt.Errorf("%d node(s) failed: %s", len(failures), strings.Join(failures, "; "))
	}
	return nil
}

// extractNamespaces walks a (multi-doc) YAML manifest and returns the
// unique set of metadata.namespace values referenced. Empty strings
// (cluster-scoped resources) are filtered out. Order is preserved so
// the caller sees a deterministic creation order in logs.
func extractNamespaces(raw []byte) []string {
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	seen := map[string]bool{}
	var out []string
	for {
		var doc struct {
			Metadata struct {
				Namespace string `yaml:"namespace"`
			} `yaml:"metadata"`
		}
		if err := dec.Decode(&doc); err != nil {
			// io.EOF or malformed YAML — stop and return what we have.
			// Apply itself will surface a malformed-YAML error if relevant.
			break
		}
		ns := strings.TrimSpace(doc.Metadata.Namespace)
		if ns == "" || seen[ns] {
			continue
		}
		seen[ns] = true
		out = append(out, ns)
	}
	return out
}
