package cluster

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"time"

	"github.com/mwiget/dpubnkctl/internal/version"
)

// CheckDocker returns nil if docker is on PATH and responds. Surfaces a
// friendly error otherwise — kubespray runs inside the bundled
// quay.io/kubespray/kubespray image so docker is a hard dependency.
func CheckDocker(ctx context.Context) error {
	if _, err := exec.LookPath("docker"); err != nil {
		return errors.New("docker not found on PATH — install Docker Engine to run kubespray")
	}
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := exec.CommandContext(checkCtx, "docker", "version", "--format", "{{.Server.Version}}").Run(); err != nil {
		return fmt.Errorf("docker daemon not responding: %w", err)
	}
	return nil
}

// PullKubespray pulls the pinned kubespray image. Streams docker pull
// output to w (one line per layer) so the operator sees progress on the
// first cluster of a session.
func PullKubespray(ctx context.Context, w io.Writer) error {
	cmd := exec.CommandContext(ctx, "docker", "pull", version.KubesprayImage)
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

	cmd := exec.CommandContext(runCtx, "docker", args...)
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
// Three paths supported:
//
//	server: https://127.0.0.1:6443          (kubespray non-LB default)
//	server: https://localhost:6443          (variant)
//	server: https://<any-IP-or-host>:6443   (kubespray LB mode — this
//	                                         is what apiserver_loadbalancer_
//	                                         domain_name produces; in our
//	                                         lake1 case it's the data-plane
//	                                         IP that the operator can't
//	                                         route to)
//
// Also adds `insecure-skip-tls-verify: true` and strips the cluster's
// certificate-authority-data because the apiserver cert SAN typically
// only includes the data-plane address + cluster.local — connecting via
// mgmt addr fails TLS otherwise. (The supplementary_addresses_in_ssl_keys
// inventory knob can extend the SAN if you'd rather use proper TLS.)
func LocalizeKubeconfig(raw, hostAddress string) string {
	target := "https://" + hostAddress + ":6443"
	// Replace any server: line with a 6443 URL.
	out := serverRe.ReplaceAllString(raw, "    server: "+target)
	// Drop CA data + insert insecure-skip-tls-verify under the same
	// cluster: stanza. Both sit at 4-space indent on contiguous lines
	// in admin.conf.
	out = caDataRe.ReplaceAllString(out, "    insecure-skip-tls-verify: true")
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
