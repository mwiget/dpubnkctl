package cli

import (
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

	"github.com/mwiget/dpubnkctl/internal/cluster"
	"github.com/mwiget/dpubnkctl/internal/deploy"
	"github.com/mwiget/dpubnkctl/internal/embedded"
	"github.com/mwiget/dpubnkctl/internal/poc"
	"github.com/mwiget/dpubnkctl/internal/ssh"
)

type deployNetworkFlags struct {
	pocDir        string
	yolo          bool
	confirmDeploy string
	skipPull      bool
	waitTimeout   time.Duration
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
	return cmd
}

// applyOrder is the f5-bnk-tested sequence — multus first (provides
// NAD CRD), then plugins, then SR-IOV, then NADs last. local-path
// provisioner appended so BNK control-plane PVCs (DSSM, observer,
// downloader, CWC, ...) bind on a fresh cluster — kubespray doesn't
// install any storage class by default.
var applyOrder = []string{
	"multus.yaml",
	"cni-plugins.yaml",
	"sriovdp-config.yaml",
	"sriov-cni-daemonset.yaml",
	"sriovdp-daemonset.yaml",
	"nad-sf.yaml",
	"local-path-provisioner.yaml",
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
		fmt.Fprintln(out, "[1/3] Tools preflight ...")
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

	fmt.Fprintf(out, "[2/3] Applying %d manifests in order ...\n", len(applyOrder))
	for _, name := range applyOrder {
		raw, err := embedded.Templates.ReadFile("templates/network/" + name)
		if err != nil {
			return fmt.Errorf("read embedded %s: %w", name, err)
		}
		if err := os.WriteFile(filepath.Join(stagedDir, name), raw, 0o644); err != nil {
			return err
		}
		fmt.Fprintf(out, "      → %s\n", name)
		// Pre-create every namespace referenced in this manifest.
		// `default` always exists, but custom-targeted NADs may reference
		// f5-operators / cert-manager / etc. before those namespaces
		// land. kubectl apply against a missing namespace fails with
		// `Error from server (NotFound): namespaces "X" not found`.
		// (AGENTS.md #14.) Idempotent — applying an existing Namespace
		// manifest is a no-op.
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
		// After multus.yaml lands, the NAD CRD is created — wait for it
		// to become Established before we apply any NADs (otherwise
		// kubectl's API discovery cache hasn't refreshed and reports
		// "no matches for kind NetworkAttachmentDefinition").
		if name == "multus.yaml" {
			if err := r.Wait(ctx, "", "Established",
				"crd/network-attachment-definitions.k8s.cni.cncf.io", 2*time.Minute); err != nil {
				fmt.Fprintf(out, "      WARN: NAD CRD not Established: %v\n", err)
			}
		}
	}
	fmt.Fprintln(out, "      all applied.")

	// 3. Wait for the daemonsets to roll out.
	fmt.Fprintln(out, "[3/3] Waiting for Multus + SR-IOV daemonsets to be Ready ...")
	rollouts := []struct {
		ns       string
		dsName   string
		friendly string
	}{
		{"kube-system", "kube-multus-ds", "Multus"},
		{"kube-system", "kube-sriov-cni-ds-amd64", "SR-IOV CNI"},
		{"kube-system", "kube-sriov-device-plugin-amd64", "SR-IOV device plugin"},
	}
	for _, ro := range rollouts {
		fmt.Fprintf(out, "      waiting on daemonset/%s ...\n", ro.dsName)
		// Poll for "all desired pods scheduled". `kubectl rollout status ds`
		// is the right idiom but our Wait wraps `kubectl wait` — use
		// status by --for=condition=Available isn't supported on DaemonSet,
		// so fall back to pod readiness via label selector.
		if err := r.Wait(ctx, ro.ns, "Ready",
			"pod", f.waitTimeout,
			"-l", "name="+ro.dsName); err != nil {
			fmt.Fprintf(out, "      WARN: %s pods not Ready within %s — `kubectl -n %s get ds %s` (%v)\n",
				ro.friendly, f.waitTimeout, ro.ns, ro.dsName, err)
		} else {
			fmt.Fprintf(out, "      %s pods Ready.\n", ro.friendly)
		}
	}

	// Make local-path the default storage class so PVCs without an
	// explicit storageClassName still bind. CNEInstance template uses
	// `storageClassName: local-path` explicitly so this is belt-
	// and-suspenders, but the cluster also benefits from a default.
	if err := r.Kubectl(ctx, "annotate", "sc", "local-path",
		"storageclass.kubernetes.io/is-default-class=true", "--overwrite"); err != nil {
		fmt.Fprintf(out, "      WARN: could not mark local-path default: %v\n", err)
	}

	// Restart containerd on every node (hosts + DPUs) so its CRI
	// re-scans /etc/cni/net.d. install-cni-plugins drops new binaries
	// (multus, calico, sriov) and rewrites configs after containerd
	// already settled — without restart, kubelet keeps logging
	// "cni plugin not initialized" until reboot. AGENTS.md #5.
	fmt.Fprintln(out, "\nRestarting containerd on every node (CRI re-scan) ...")
	if err := restartContainerdEverywhere(ctx, repo, p, out); err != nil {
		fmt.Fprintf(out, "      WARN: containerd restart had errors (some nodes may stay NotReady): %v\n", err)
	}

	// The containerd restart kills every pod's connection to its CRI
	// shim; the kubelet rebuilds them on the next sync, which briefly
	// flips affected DaemonSets to ImagePullBackOff / NotReady before
	// recovering. If `deploy network` returns while that's still
	// flapping, the operator (or the next phase) reads the noise as
	// "deploy network failed". Wait for the DaemonSets we explicitly
	// rely on to come back to full Ready before declaring done.
	// `kubectl rollout status ds/...` blocks until
	// numberReady == desiredNumberScheduled.
	fmt.Fprintln(out, "\nWaiting for DaemonSets to converge after containerd restart ...")
	for _, ds := range networkDaemonSets {
		fmt.Fprintf(out, "      %s/%s ...\n", ds.namespace, ds.name)
		if err := r.Kubectl(ctx, "rollout", "status",
			"-n", ds.namespace,
			"ds/"+ds.name, "--timeout=3m"); err != nil {
			// Don't fail the phase: a DS that isn't installed yet
			// (timing on the last apply) or a transient straggler after
			// the bounce each surface here. Warn so the operator can
			// triage if they want, but let the deploy chain proceed.
			fmt.Fprintf(out, "      WARN: %s/%s not fully Ready: %v\n", ds.namespace, ds.name, err)
		}
	}

	// Detect-and-fix the multus first-start CNI race. On a healthy
	// cluster this is a 1-second probe and a no-op.
	//
	// Background: a multus pod that boots before calico's install-cni
	// initContainer drops /etc/cni/net.d/10-calico.conflist records
	// loopback-only delegates in /etc/cni/net.d/00-multus.conf and
	// never updates. Every subsequent pod on that node hangs
	// ContainerCreating with `multus ... missing network name`.
	//
	// Signal: after the standard DS-converge step above, any pod in
	// kube-system stuck in Pending phase is downstream of the race
	// (sriov-cni-ds or sriov-device-plugin pods on the broken node).
	// Healthy cluster → no Pending pods → no rotation needed.
	//
	// Recovery: rotate multus + both sriov DSes (so the wedged sriov
	// pods get a fresh sandbox-creation attempt against the now-
	// correct multus delegate; kubelet CRI backoff doesn't auto-
	// recover them), then re-check. (AGENTS.md #26.)
	fmt.Fprintln(out, "\nProbing for multus first-start CNI race ...")
	stuck, _ := r.KubectlCapture(ctx, "-n", "kube-system", "get", "pods",
		"--field-selector=status.phase=Pending",
		"-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
	stuck = strings.TrimSpace(stuck)
	if stuck == "" {
		fmt.Fprintln(out, "      no Pending pods in kube-system — CNI healthy.")
	} else {
		fmt.Fprintf(out, "      detected Pending pods (race triggered):\n%s\n", indent(stuck, "        "))
		fmt.Fprintln(out, "      Rotating multus + sriov daemonsets to recover ...")
		// DS names match the embedded manifests in
		// internal/embedded/templates/network/. They are arch-agnostic
		// (multus.yaml + sriov-*-daemonset.yaml use nodeAffinity, not
		// arch-suffixed names) — DPU nodes are arm64, host nodes amd64,
		// both run the same DS. Previous code carried `-amd64`-suffixed
		// names from an earlier kubespray naming convention; on a mixed-
		// arch cluster the rotation silently no-op'd against names that
		// never existed and the broken multus stayed broken until the
		// CNEInstance-Available wait timed out 15 min later. (Caught on
		// the wizard-deploy May 16 measurement run.)
		cniDS := []string{
			"kube-multus-ds",
			"kube-sriov-cni-ds",
			"kube-sriov-device-plugin",
		}
		for _, ds := range cniDS {
			if err := r.Kubectl(ctx, "rollout", "restart",
				"-n", "kube-system", "ds/"+ds); err != nil {
				fmt.Fprintf(out, "      WARN: could not restart %s: %v\n", ds, err)
			}
		}
		for _, ds := range cniDS {
			if err := r.Kubectl(ctx, "rollout", "status",
				"-n", "kube-system", "ds/"+ds, "--timeout=5m"); err != nil {
				fmt.Fprintf(out, "      WARN: %s rotation did not converge: %v\n", ds, err)
			}
		}
		// Sweep any pod still wedged in Pending after the rotation
		// (its broken sandbox is older than the rotation; kubelet
		// won't retry until the backoff clock expires).
		stuck2, _ := r.KubectlCapture(ctx, "get", "pods", "-A",
			"--field-selector=status.phase=Pending",
			"-o", "jsonpath={range .items[*]}{.metadata.namespace}{\" \"}{.metadata.name}{\"\\n\"}{end}")
		for _, line := range strings.Split(strings.TrimSpace(stuck2), "\n") {
			f := strings.Fields(line)
			if len(f) != 2 {
				continue
			}
			_ = r.Kubectl(ctx, "-n", f[0], "delete", "pod", f[1],
				"--ignore-not-found", "--wait=false")
		}
		fmt.Fprintln(out, "      CNI rotation complete.")
	}

	// Verify the recovery actually worked. Without this, a rotation
	// against a wrong DS name (or a deeper multus issue) silently
	// no-op'd and downstream BNK install proceeded against a broken
	// cluster — TMM lands only on the nodes where multus is healthy,
	// CNEInstance Available wait at deploy-cne times out 15 min later
	// with an opaque "0 out of N pods" message. Probe again for Pending
	// pods after a brief settle; if any remain, fail loudly with a
	// pointer at the offending pods.
	fmt.Fprintln(out, "\nVerifying multus is healthy on every node ...")
	time.Sleep(10 * time.Second)
	residual, _ := r.KubectlCapture(ctx, "get", "pods", "-A",
		"--field-selector=status.phase=Pending",
		"-o", "jsonpath={range .items[*]}{.metadata.namespace}{\"/\"}{.metadata.name}{\" on \"}{.spec.nodeName}{\"\\n\"}{end}")
	residual = strings.TrimSpace(residual)
	if residual != "" {
		return fmt.Errorf("multus race not recovered — pods still Pending after rotation:\n%s\n\nInspect with `kubectl describe pod -n <ns> <pod>`; the multus error is typically `missing network name` from a stale /etc/cni/net.d/00-multus.conf on the affected node. Manually rotate with `kubectl rollout restart -n kube-system ds/kube-multus-ds`.", indent(residual, "  "))
	}
	fmt.Fprintln(out, "      every pod scheduled; multus delegate chain intact.")

	appendDeployJournal(repo, p.Metadata.Name, "", "NETWORK INSTALLED", "")
	fmt.Fprintln(out, "\nDONE.  FLO should now reconcile the CNEInstance — re-check `kubectl get cneinstance -A` + `kubectl get pods -A`.")
	return nil
}

// networkDaemonSets lists the DaemonSets `deploy network` apply
// that should be Ready before the phase returns. Same set we wait
// on earlier in this command (multus + sriov) plus local-path,
// which also bounces during the containerd restart and is the
// default storage class for the rest of the deploy.
var networkDaemonSets = []struct {
	namespace string
	name      string
}{
	{"kube-system", "kube-multus-ds"},
	{"kube-system", "kube-sriov-cni-ds-amd64"},
	{"kube-system", "kube-sriov-device-plugin-amd64"},
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
