package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mwiget/dpubnkctl/internal/poc"
	"github.com/mwiget/dpubnkctl/internal/version"
)

func newClusterStatusCmd() *cobra.Command {
	var pocDir string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show PoC phase + cluster node readiness + BNK platform health",
		Long: `Read-only health view of the PoC. Reports:

  1. poc.yaml.status (phase tracker)
  2. Node Ready states (kubectl get nodes)
  3. kube-system component pod health (etcd, apiserver, scheduler, …)
  4. CNI / Multus / SR-IOV daemonset rollout
  5. FLO controller deployment (if installed)
  6. CNEInstance readiness (if deployed)
  7. Any pod not Running or Completed cluster-wide

Degrades cleanly when earlier phases haven't run: a missing kubeconfig
or namespace surfaces as "not deployed yet", not an error.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runClusterStatus(cmd.Context(), cmd.OutOrStdout(), pocDir)
		},
	}
	cmd.Flags().StringVar(&pocDir, "poc", "", "PoC repo path (default: current directory)")
	return cmd
}

func runClusterStatus(ctx context.Context, out io.Writer, pocDir string) error {
	repo, err := resolvePoCDir(pocDir)
	if err != nil {
		return err
	}
	p, err := poc.Load(repo)
	if err != nil {
		return fmt.Errorf("not a PoC repo (%s): %w", repo, err)
	}

	fmt.Fprintf(out, "PoC: %s   (BNK %s)\n\n", p.Metadata.Name, p.Metadata.BNKVersion)

	// 1. Phase tracker from poc.yaml.status.
	fmt.Fprintln(out, "Phase status (from poc.yaml):")
	fmt.Fprintf(out, "  discover  : %s\n", statusOrPending(p.Status.Discover))
	fmt.Fprintf(out, "  provision : %s\n", statusOrPending(p.Status.Provision))
	fmt.Fprintf(out, "  cluster   : %s\n", statusOrPending(p.Status.Cluster))
	fmt.Fprintf(out, "  deploy    : %s\n", statusOrPending(p.Status.Deploy))
	if !p.Status.LastPhaseAt.IsZero() {
		fmt.Fprintf(out, "  last phase at: %s\n", p.Status.LastPhaseAt.UTC().Format(time.RFC3339))
	}
	fmt.Fprintln(out)

	// 2. Cluster reach.
	kubeconfig := filepath.Join(repo, "artifacts", "kubeconfig")
	if _, err := os.Stat(kubeconfig); err != nil {
		fmt.Fprintf(out, "No kubeconfig at %s — `dpubnkctl cluster up` hasn't completed yet.\n", kubeconfig)
		return nil
	}
	k := &kubectlReader{kubeconfig: kubeconfig}
	if err := k.preflight(ctx); err != nil {
		fmt.Fprintf(out, "Container runtime or kubectl unreachable: %v\n", err)
		return nil
	}

	// 3. Nodes.
	fmt.Fprintln(out, "Nodes (kubectl get nodes -o wide):")
	if err := k.runStream(ctx, out, "      | ", "get", "nodes", "-o", "wide"); err != nil {
		fmt.Fprintf(out, "      (failed: %v)\n", err)
	}
	fmt.Fprintln(out)

	// 4. kube-system component pods (etcd / apiserver / scheduler / controller-manager
	//    are all static pods named with control-plane host suffixes — match by labels).
	fmt.Fprintln(out, "Control-plane components (kube-system):")
	if err := k.runStream(ctx, out, "      | ",
		"-n", "kube-system", "get", "pods",
		"-l", "tier=control-plane",
		"-o", "wide"); err != nil {
		fmt.Fprintf(out, "      (failed: %v)\n", err)
	}
	fmt.Fprintln(out)

	// 5. CNI / Multus / SR-IOV daemonsets — sentinel for `deploy network`.
	fmt.Fprintln(out, "Network plugins (kube-system daemonsets):")
	if err := k.runStream(ctx, out, "      | ",
		"-n", "kube-system", "get", "ds",
		"kube-multus-ds",
		"kube-sriov-cni-ds-amd64",
		"kube-sriov-device-plugin-amd64",
		"--ignore-not-found",
		"-o", "wide"); err != nil {
		fmt.Fprintf(out, "      (some daemonsets may not be installed: %v)\n", err)
	}
	fmt.Fprintln(out)

	// 6. FLO controller deployment.
	fmt.Fprintln(out, "F5 Lifecycle Operator (f5-operators):")
	if has, _ := k.namespaceExists(ctx, "f5-operators"); has {
		_ = k.runStream(ctx, out, "      | ",
			"-n", "f5-operators", "get", "deploy",
			"-l", "app.kubernetes.io/name=f5-lifecycle-operator",
			"-o", "wide")
	} else {
		fmt.Fprintln(out, "      (f5-operators namespace not present — `deploy flo` not run yet)")
	}
	fmt.Fprintln(out)

	// 7. CNEInstance — exists in user namespace once `deploy cne` runs.
	fmt.Fprintln(out, "CNEInstance (BNK platform):")
	if has, _ := k.crdEstablished(ctx, "cneinstances.f5bigip.f5net.com"); has {
		_ = k.runStream(ctx, out, "      | ", "get", "cneinstance", "-A")
	} else {
		fmt.Fprintln(out, "      (CNEInstance CRD not installed — `deploy flo` not run yet)")
	}
	fmt.Fprintln(out)

	// 8. Anything unhealthy cluster-wide. `kubectl get pods -A
	//    --field-selector=status.phase!=Running,status.phase!=Succeeded`
	//    catches Pending / Failed / CrashLoopBackOff. Headers off so an
	//    empty result is a clean blank.
	fmt.Fprintln(out, "Unhealthy pods (not Running/Succeeded, cluster-wide):")
	stdout, err := k.run(ctx,
		"get", "pods", "-A",
		"--field-selector=status.phase!=Running,status.phase!=Succeeded",
		"--no-headers")
	if err != nil {
		fmt.Fprintf(out, "      (query failed: %v)\n", err)
	} else if strings.TrimSpace(stdout) == "" {
		fmt.Fprintln(out, "      none — all pods Running or Succeeded.")
	} else {
		for _, line := range strings.Split(strings.TrimRight(stdout, "\n"), "\n") {
			fmt.Fprintf(out, "      | %s\n", line)
		}
	}
	return nil
}

// kubectlReader wraps a few read-only kubectl invocations against a
// kubeconfig, using the same alpine/k8s docker container the deploy
// runner uses so we don't introduce a new dependency surface.
type kubectlReader struct {
	kubeconfig string
}

// preflight ensures docker (or podman) is up and the kubeconfig parses
// well enough to talk to the apiserver. Used to short-circuit with a
// friendly message before running the rest of the status checks.
func (k *kubectlReader) preflight(ctx context.Context) error {
	rt := containerRuntime(ctx)
	if rt == "" {
		return fmt.Errorf("no container runtime (install docker or podman; see `dpubnkctl doctor`)")
	}
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(pctx, rt,
		"run", "--rm",
		"-v", k.kubeconfig+":/kubeconfig:ro",
		"--network=host",
		"-e", "KUBECONFIG=/kubeconfig",
		version.K8sToolsImage,
		"kubectl", "cluster-info")
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	return cmd.Run()
}

// run captures kubectl stdout (suppresses stderr). Used when the caller
// wants to inspect the result rather than show it to the operator.
func (k *kubectlReader) run(ctx context.Context, args ...string) (string, error) {
	rt := containerRuntime(ctx)
	if rt == "" {
		return "", fmt.Errorf("no container runtime")
	}
	full := []string{
		"run", "--rm",
		"-v", k.kubeconfig + ":/kubeconfig:ro",
		"--network=host",
		"-e", "KUBECONFIG=/kubeconfig",
		version.K8sToolsImage,
		"kubectl",
	}
	full = append(full, args...)
	cmd := exec.CommandContext(ctx, rt, full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("kubectl %s: %w (stderr: %s)",
			strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// runStream streams kubectl output to w with a prefix; useful for the
// "show me the table" calls. Mirrors the prefix convention the deploy
// commands use.
func (k *kubectlReader) runStream(ctx context.Context, w io.Writer, prefix string, args ...string) error {
	out, err := k.run(ctx, args...)
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) == "" {
		fmt.Fprintf(w, "%s(empty)\n", prefix)
		return nil
	}
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		fmt.Fprintf(w, "%s%s\n", prefix, line)
	}
	return nil
}

func (k *kubectlReader) namespaceExists(ctx context.Context, ns string) (bool, error) {
	_, err := k.run(ctx, "get", "namespace", ns, "--no-headers", "--ignore-not-found")
	if err != nil {
		return false, err
	}
	// `--ignore-not-found` returns empty stdout when missing, exit 0.
	// To distinguish, re-check with a get that would error on miss.
	_, err = k.run(ctx, "get", "namespace", ns, "-o", "name")
	return err == nil, nil
}

func (k *kubectlReader) crdEstablished(ctx context.Context, name string) (bool, error) {
	_, err := k.run(ctx, "get", "crd", name, "-o", "name")
	return err == nil, nil
}

// containerRuntime returns "docker" or "podman" — whichever is on PATH
// and responds to `version`. Returns "" if neither. We re-derive here
// instead of going through cluster.CheckContainerRuntime to avoid the
// log spam — status is silent on the happy path.
func containerRuntime(ctx context.Context) string {
	for _, rt := range []string{"docker", "podman"} {
		if _, err := exec.LookPath(rt); err != nil {
			continue
		}
		pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err := exec.CommandContext(pctx, rt, "version", "--format", "{{.Server.Version}}").Run()
		cancel()
		if err == nil {
			return rt
		}
	}
	return ""
}
