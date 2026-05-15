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

// KubectlCapture runs kubectl and returns stdout WITHOUT streaming it
// to r.Out. Useful when the caller wants to consume the output
// programmatically (parse names, count rows, etc.) without spamming
// the operator's terminal. Stderr is captured into the returned error.
func (r *Runner) KubectlCapture(ctx context.Context, args ...string) (string, error) {
	full := r.kubectlArgs(args...)
	cmd := exec.CommandContext(ctx, "docker", full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("kubectl %s: %w (stderr: %s)",
			strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// Wait runs `kubectl wait` against an arbitrary selector with optional
// extra flags (e.g. -l app=foo). Useful for rollout completion when the
// resource name is chart-prefixed and unknown a priori.
func (r *Runner) Wait(ctx context.Context, namespace, condition, selector string, timeout time.Duration, extraArgs ...string) error {
	args := r.kubectlArgs("wait")
	if namespace != "" {
		args = append(args, "-n", namespace)
	}
	args = append(args, "--for=condition="+condition,
		"--timeout="+timeout.String(),
		selector)
	args = append(args, extraArgs...)
	cmd := exec.CommandContext(ctx, "docker", args...)
	var out bytes.Buffer
	cmd.Stdout = io.MultiWriter(r.Out, &out)
	cmd.Stderr = cmd.Stdout
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kubectl wait %s: %w\n%s", selector, err, out.String())
	}
	return nil
}

// OCIAuth carries the credentials for an authenticated OCI helm chart
// pull. RegistryHost is the bare hostname (e.g. "repo.f5.com").
// Username/Password go to `helm registry login`.
type OCIAuth struct {
	RegistryHost string
	Username     string
	Password     string
}

// HelmUpgradeOCI is a variant of HelmUpgrade that runs `helm registry
// login` and then `helm upgrade` inside the same container so the
// docker --rm doesn't lose the login state between invocations.
func (r *Runner) HelmUpgradeOCI(ctx context.Context, auth OCIAuth, release, ociChart, namespace, chartVersion, valuesYAML string, extraArgs ...string) error {
	dockerArgs := []string{
		"run", "--rm", "-i",
		"-v", r.KubeconfigPath + ":/kubeconfig:ro",
		"--network=host",
		"-e", "KUBECONFIG=/kubeconfig",
	}
	if valuesYAML != "" {
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
		dockerArgs = append(dockerArgs, "-v", tmp.Name()+":/values.yaml:ro")
	}
	// Build the upgrade command tail.
	upgradeArgs := []string{
		"helm", "upgrade", "--install", release, ociChart,
		"--namespace", namespace, "--create-namespace",
		"--wait", "--timeout=10m",
	}
	if chartVersion != "" {
		upgradeArgs = append(upgradeArgs, "--version", chartVersion)
	}
	if valuesYAML != "" {
		upgradeArgs = append(upgradeArgs, "-f", "/values.yaml")
	}
	upgradeArgs = append(upgradeArgs, extraArgs...)

	// Compose: login (password from stdin) && upgrade.
	//
	// We feed the registry password through the docker container's stdin
	// — NOT into the script body — so it never appears on the host's
	// argv list (visible via `ps`/`auditd`/Falco). The script reads
	// stdin with `cat` (NOT `read -r`, which truncates multi-line input
	// to the first line — a GCP service account JSON is pretty-printed
	// multi-line, so `read -r PW` reduced the password to literally `{`
	// and helm got a 401), pipes the full body to helm's
	// `--password-stdin`. The script template itself only contains
	// registry host, username, and upgrade args (no secrets).
	script := fmt.Sprintf(
		`cat | helm registry login %s --username %s --password-stdin && %s`,
		auth.RegistryHost, auth.Username,
		strings.Join(upgradeArgs, " "))

	dockerArgs = append(dockerArgs, version.K8sToolsImage, "sh", "-c", script)
	cmd := exec.CommandContext(ctx, "docker", dockerArgs...)
	cmd.Stdin = strings.NewReader(auth.Password + "\n")
	var out bytes.Buffer
	cmd.Stdout = io.MultiWriter(r.Out, &out)
	cmd.Stderr = cmd.Stdout
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("helm upgrade %s (oci): %w\n%s", release, err, out.String())
	}
	return nil
}

// HelmUpgrade installs or upgrades a release. Pass repoURL non-empty for
// HTTP charts (uses --repo so we don't need persistent helm repo state
// across docker-run invocations); leave empty for OCI charts (chart name
// is the full oci:// URL).
//
// valuesYAML is optional. extraArgs are appended verbatim to helm.
func (r *Runner) HelmUpgrade(ctx context.Context, release, chart, repoURL, namespace, chartVersion, valuesYAML string, extraArgs ...string) error {
	dockerArgs := []string{
		"run", "--rm",
		"-v", r.KubeconfigPath + ":/kubeconfig:ro",
		"--network=host",
		"-e", "KUBECONFIG=/kubeconfig",
	}

	if valuesYAML != "" {
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
		dockerArgs = append(dockerArgs, "-v", tmp.Name()+":/values.yaml:ro")
	}

	dockerArgs = append(dockerArgs, version.K8sToolsImage, "helm",
		"upgrade", "--install", release, chart,
		"--namespace", namespace, "--create-namespace",
		"--wait", "--timeout=10m")
	if repoURL != "" {
		dockerArgs = append(dockerArgs, "--repo", repoURL)
	}
	if chartVersion != "" {
		dockerArgs = append(dockerArgs, "--version", chartVersion)
	}
	if valuesYAML != "" {
		dockerArgs = append(dockerArgs, "-f", "/values.yaml")
	}
	dockerArgs = append(dockerArgs, extraArgs...)

	cmd := exec.CommandContext(ctx, "docker", dockerArgs...)
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

// Helm runs a generic helm subcommand (uninstall, list, status, ...).
// The kubeconfig is mounted read-only and KUBECONFIG points at it.
// Output is streamed through r.Out and captured for the error message.
func (r *Runner) Helm(ctx context.Context, args ...string) error {
	full := r.helmArgs(args...)
	cmd := exec.CommandContext(ctx, "docker", full...)
	var out bytes.Buffer
	cmd.Stdout = io.MultiWriter(r.Out, &out)
	cmd.Stderr = cmd.Stdout
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("helm %s: %w\n%s", strings.Join(args, " "), err, out.String())
	}
	return nil
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
