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
	if !f.yolo {
		return errors.New("refusing destructive deploy without --yolo")
	}
	if f.confirmDeploy != p.Metadata.Name {
		return fmt.Errorf("--confirm-deploy must equal poc.yaml.metadata.name (%q), got %q", p.Metadata.Name, f.confirmDeploy)
	}
	kubeconfig := filepath.Join(repo, "artifacts", "kubeconfig")
	if _, err := os.Stat(kubeconfig); err != nil {
		return fmt.Errorf("kubeconfig %s missing — run `dpubnkctl cluster up` first", kubeconfig)
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

	appendDeployJournal(repo, p.Metadata.Name, "", "NETWORK INSTALLED", "")
	fmt.Fprintln(out, "\nDONE.  FLO should now reconcile the CNEInstance — re-check `kubectl get cneinstance -A` + `kubectl get pods -A`.")
	return nil
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
		hostKey := h.SSH.KeyRef
		if !filepath.IsAbs(hostKey) {
			hostKey = filepath.Join(repo, hostKey)
		}
		known := filepath.Join(repo, "inventory", "known_hosts")
		for j := range h.DPUs {
			d := &h.DPUs[j]
			if d.TmfifoIP == "" || d.Hostname == "" {
				continue
			}
			dpuIP := strings.SplitN(d.TmfifoIP, "/", 2)[0]
			jobs = append(jobs, job{
				label: d.Hostname,
				cfg: ssh.Config{
					Address: dpuIP, Port: 22, User: "ubuntu",
					KeyPath: hostKey, Timeout: 30 * time.Second,
					Jumphost: &ssh.Config{
						Address: h.SSH.Address, Port: h.SSH.Port, User: h.SSH.User,
						KeyPath: hostKey, KnownHosts: known, Timeout: 30 * time.Second,
					},
				},
			})
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
