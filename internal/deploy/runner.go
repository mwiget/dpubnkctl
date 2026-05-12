// Package deploy installs the BNK platform on top of a kubespray-built
// cluster: namespaces, FAR pull secret, cert-manager, FLO, CNEInstance,
// VLAN CRs, GatewayClass.
//
// kubectl and helm are run inside their pinned Docker images so the
// operator's laptop only needs Docker (already required for kubespray).
package deploy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/mwiget/dpubnkctl/internal/version"
)

// Runner wraps the small kubectl/helm surface dpubnkctl needs.
// Kubeconfig is bind-mounted at /kubeconfig inside both containers so
// callers don't have to think about paths.
type Runner struct {
	KubeconfigPath string    // host-side path to PoC's kubeconfig
	Out            io.Writer // streamed kubectl/helm output
}

// CheckTools verifies docker is up and pulls the kubectl + helm images.
func (r *Runner) CheckTools(ctx context.Context) error {
	if _, err := exec.LookPath("docker"); err != nil {
		return errors.New("docker not found on PATH")
	}
	if _, err := os.Stat(r.KubeconfigPath); err != nil {
		return fmt.Errorf("kubeconfig %s: %w", r.KubeconfigPath, err)
	}
	for _, img := range []string{version.K8sToolsImage, version.K8sToolsImage} {
		fmt.Fprintf(r.Out, "      pulling %s ...\n", img)
		c := exec.CommandContext(ctx, "docker", "pull", img)
		c.Stdout, c.Stderr = io.Discard, io.Discard
		if err := c.Run(); err != nil {
			return fmt.Errorf("docker pull %s: %w", img, err)
		}
	}
	return nil
}

// Apply pipes manifest YAML to `kubectl apply -f -`.
func (r *Runner) Apply(ctx context.Context, manifest string) error {
	args := r.kubectlArgs("apply", "-f", "-")
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdin = strings.NewReader(manifest)
	var out bytes.Buffer
	cmd.Stdout = io.MultiWriter(r.Out, &out)
	cmd.Stderr = cmd.Stdout
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kubectl apply: %w\n%s", err, out.String())
	}
	return nil
}

// Kubectl runs an arbitrary kubectl subcommand. Errors include the
// captured output for diagnostics.
func (r *Runner) Kubectl(ctx context.Context, args ...string) error {
	full := r.kubectlArgs(args...)
	cmd := exec.CommandContext(ctx, "docker", full...)
	var out bytes.Buffer
	cmd.Stdout = io.MultiWriter(r.Out, &out)
	cmd.Stderr = cmd.Stdout
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kubectl %s: %w\n%s", strings.Join(args, " "), err, out.String())
	}
	return nil
}

// Wait runs `kubectl wait` against an arbitrary selector. Useful for
// rollout completion (cert-manager webhook, FLO CRDs, etc.).
func (r *Runner) Wait(ctx context.Context, namespace, condition, selector string, timeout time.Duration) error {
	args := r.kubectlArgs("wait")
	if namespace != "" {
		args = append(args, "-n", namespace)
	}
	args = append(args, "--for=condition="+condition,
		"--timeout="+timeout.String(),
		selector)
	cmd := exec.CommandContext(ctx, "docker", args...)
	var out bytes.Buffer
	cmd.Stdout = io.MultiWriter(r.Out, &out)
	cmd.Stderr = cmd.Stdout
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kubectl wait %s: %w\n%s", selector, err, out.String())
	}
	return nil
}

// HelmRepoAdd is a no-op for OCI charts; for HTTP repos it adds + updates.
func (r *Runner) HelmRepoAdd(ctx context.Context, name, url string) error {
	if strings.HasPrefix(url, "oci://") {
		return nil
	}
	args := r.helmArgs("repo", "add", name, url)
	cmd := exec.CommandContext(ctx, "docker", args...)
	var out bytes.Buffer
	cmd.Stdout = io.MultiWriter(r.Out, &out)
	cmd.Stderr = cmd.Stdout
	if err := cmd.Run(); err != nil {
		// Already-exists is not fatal.
		if !strings.Contains(out.String(), "already exists") {
			return fmt.Errorf("helm repo add %s: %w\n%s", name, err, out.String())
		}
	}
	cmd2 := exec.CommandContext(ctx, "docker", r.helmArgs("repo", "update")...)
	cmd2.Stdout, cmd2.Stderr = io.Discard, io.Discard
	return cmd2.Run()
}

// HelmUpgrade installs or upgrades a release. valuesYAML may be empty.
func (r *Runner) HelmUpgrade(ctx context.Context, release, chart, namespace, chartVersion, valuesYAML string, extraArgs ...string) error {
	args := r.helmArgs("upgrade", "--install", release, chart,
		"--namespace", namespace, "--create-namespace",
		"--wait", "--timeout=10m")
	if chartVersion != "" {
		args = append(args, "--version", chartVersion)
	}
	if valuesYAML != "" {
		// Stage values file at /values.yaml inside the helm container.
		tmp, err := os.CreateTemp("", "dpubnkctl-helm-values-*.yaml")
		if err != nil {
			return err
		}
		defer os.Remove(tmp.Name())
		if _, err := tmp.WriteString(valuesYAML); err != nil {
			tmp.Close()
			return err
		}
		tmp.Close()
		// Replace the helm-args mount list to also include the values file.
		args = append(args[:0],
			"run", "--rm",
			"-v", r.KubeconfigPath+":/kubeconfig:ro",
			"-v", tmp.Name()+":/values.yaml:ro",
			"--network=host",
			"-e", "KUBECONFIG=/kubeconfig",
			version.K8sToolsImage,
			"helm",
			"upgrade", "--install", release, chart,
			"--namespace", namespace, "--create-namespace",
			"--wait", "--timeout=10m",
			"-f", "/values.yaml",
		)
		if chartVersion != "" {
			args = append(args, "--version", chartVersion)
		}
	}
	args = append(args, extraArgs...)
	cmd := exec.CommandContext(ctx, "docker", args...)
	var out bytes.Buffer
	cmd.Stdout = io.MultiWriter(r.Out, &out)
	cmd.Stderr = cmd.Stdout
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("helm upgrade %s: %w\n%s", release, err, out.String())
	}
	return nil
}

// kubectlArgs builds `docker run ... alpine/k8s ... kubectl ...`. The
// alpine/k8s image bundles kubectl + helm + others; entrypoint is /bin/sh,
// so we explicitly invoke the binary we want.
func (r *Runner) kubectlArgs(kubectlArgs ...string) []string {
	base := []string{
		"run", "--rm", "-i",
		"-v", r.KubeconfigPath + ":/kubeconfig:ro",
		"--network=host",
		"-e", "KUBECONFIG=/kubeconfig",
		version.K8sToolsImage,
		"kubectl",
	}
	return append(base, kubectlArgs...)
}

func (r *Runner) helmArgs(helmArgs ...string) []string {
	base := []string{
		"run", "--rm",
		"-v", r.KubeconfigPath + ":/kubeconfig:ro",
		"--network=host",
		"-e", "KUBECONFIG=/kubeconfig",
		version.K8sToolsImage,
		"helm",
	}
	return append(base, helmArgs...)
}

// guards against accidental absolute-path duplication when the operator
// passes a relative kubeconfig path.
func (r *Runner) absKubeconfig(repo string) (string, error) {
	if filepath.IsAbs(r.KubeconfigPath) {
		return r.KubeconfigPath, nil
	}
	return filepath.Abs(filepath.Join(repo, r.KubeconfigPath))
}
