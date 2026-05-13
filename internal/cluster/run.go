package cluster

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"time"

	"github.com/mwiget/dpubnkctl/internal/version"
)

// CheckContainerRuntime probes for docker first, then podman. Returns the
// runtime name ("docker" or "podman") on success. kubespray's container
// runs identically under both — `--rm`, `-v`, and `--network=host` are
// supported by either.
//
// docker is preferred because it's been the tested path; podman is the
// rootless-friendly fallback for hosts where Docker Engine isn't
// installable (e.g. RHEL workstations, lab gateways without root).
func CheckContainerRuntime(ctx context.Context) (string, error) {
	for _, candidate := range []struct {
		name       string
		versionCmd []string
	}{
		// `docker version --format` returns non-zero when the daemon is
		// unreachable, which is exactly what we want to detect.
		{"docker", []string{"version", "--format", "{{.Server.Version}}"}},
		// podman doesn't need a daemon by default (forked exec model);
		// `podman version` confirms the binary runs at all.
		{"podman", []string{"version", "--format", "{{.Server.Version}}"}},
	} {
		if _, err := exec.LookPath(candidate.name); err != nil {
			continue
		}
		checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := exec.CommandContext(checkCtx, candidate.name, candidate.versionCmd...).Run()
		cancel()
		if err == nil {
			return candidate.name, nil
		}
	}
	return "", errors.New("no container runtime found — install Docker Engine or Podman to run kubespray")
}

// PullKubespray pulls the pinned kubespray image using the detected
// runtime. Streams output to w (one line per layer) so the operator sees
// progress on the first cluster of a session.
func PullKubespray(ctx context.Context, w io.Writer) error {
	runtime, err := CheckContainerRuntime(ctx)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, runtime, "pull", version.KubesprayImage)
	cmd.Stdout = w
	cmd.Stderr = w
	return cmd.Run()
}

// RunOptions controls the kubespray invocation.
type RunOptions struct {
	InventoryDir string        // host-side path (will be bind-mounted read-only)
	Out          io.Writer     // streamed playbook output
	Timeout      time.Duration // wall-clock cap
	Playbook     string        // default: "cluster.yml"
	ExtraArgs    []string      // appended to ansible-playbook
}

// RunKubespray executes `ansible-playbook -i /inventory/hosts.yml
// <playbook>` inside the kubespray container. Mounts the inventory tree
// read-only at /inventory and the operator's ~/.ssh read-only at
// /root/.ssh so kubespray can reach every host.
//
// Returns the docker exit code; non-zero is reported but not turned into
// an error — the caller decides whether that's fatal.
func RunKubespray(ctx context.Context, opts RunOptions) (int, error) {
	if opts.Playbook == "" {
		opts.Playbook = "cluster.yml"
	}
	if opts.Timeout == 0 {
		opts.Timeout = 90 * time.Minute
	}

	if !filepath.IsAbs(opts.InventoryDir) {
		abs, err := filepath.Abs(opts.InventoryDir)
		if err != nil {
			return -1, err
		}
		opts.InventoryDir = abs
	}

	runtime, err := CheckContainerRuntime(ctx)
	if err != nil {
		return -1, err
	}

	args := []string{
		"run", "--rm",
		"-v", opts.InventoryDir + ":/inventory:ro",
		// Allow the container's apt/yum/etc. to reach the internet without
		// a CNI conflict if the host has restrictive defaults.
		"--network=host",
		version.KubesprayImage,
		"ansible-playbook",
		"-i", "/inventory/hosts.yml",
		"--become", "--become-user=root",
	}
	args = append(args, opts.ExtraArgs...)
	args = append(args, opts.Playbook)

	runCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, runtime, args...)
	cmd.Stdout = opts.Out
	cmd.Stderr = opts.Out

	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode(), nil
		}
		return -1, err
	}
	return 0, nil
}

// LocalizeKubeconfig rewrites the server URL in /etc/kubernetes/admin.conf
// to point at hostAddress (typically the host's SSH/mgmt address) so the
// kubeconfig works from the operator laptop.
//
// Three server: line shapes supported in admin.conf:
//
//	server: https://127.0.0.1:6443          (kubespray non-LB default)
//	server: https://localhost:6443          (variant)
//	server: https://<any-IP-or-host>:6443   (kubespray LB mode — what
//	                                         apiserver_loadbalancer_
//	                                         domain_name produces; in our
//	                                         lake1 case it's the data-plane
//	                                         IP the operator can't route to)
//
// When insecure is true (legacy / fallback): also strips the cluster's
// certificate-authority-data and inserts insecure-skip-tls-verify: true.
// Use this when the apiserver cert SAN doesn't include hostAddress —
// typically the case when poc.yaml.network.cluster_apiserver_address is
// unset, so the kubespray inventory doesn't add mgmt addresses to
// supplementary_addresses_in_ssl_keys.
//
// When insecure is false: only the server URL is rewritten. The CA data
// stays; kubectl verifies the apiserver cert normally. This is the
// preferred mode for new PoCs — the inventory render already extends the
// SAN with every host's SSH address when cluster_apiserver_address is set.
func LocalizeKubeconfig(raw, hostAddress string, insecure bool) string {
	target := "https://" + hostAddress + ":6443"
	out := serverRe.ReplaceAllString(raw, "    server: "+target)
	if insecure {
		// Drop CA data + insert insecure-skip-tls-verify under the same
		// cluster: stanza. Both sit at 4-space indent on contiguous lines
		// in admin.conf.
		out = caDataRe.ReplaceAllString(out, "    insecure-skip-tls-verify: true")
	}
	return out
}

// match `    server: https://...:6443` (any host) — kubespray emits this
// at exactly 4-space indent under `clusters[].cluster`.
var serverRe = regexp.MustCompile(`(?m)^    server: https://[^[:space:]]+:6443\s*$`)

// match the single-line CA data field — kubespray emits it base64-blob
// on one line at 4-space indent.
var caDataRe = regexp.MustCompile(`(?m)^    certificate-authority-data:.*$`)

// SaveKubeconfig writes content to dst with mode 0600 (sensitive).
func SaveKubeconfig(dst, content string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, []byte(content), 0o600)
}
