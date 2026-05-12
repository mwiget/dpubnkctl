package cluster

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/mwiget/dpubnkctl/internal/ssh"
)

// JoinCommand is the result of `kubeadm token create --print-join-command`
// run on a control-plane host. We re-use the same command on every DPU.
type JoinCommand struct {
	Raw string // verbatim line, e.g. `kubeadm join 192.168.68.66:6443 --token X --discovery-token-ca-cert-hash sha256:Y`
}

// FetchJoinCommand SSHes to the given control-plane host and asks kubeadm
// for a fresh worker join command. The token has a short TTL (default
// 24h) — fetch close in time to actually using it.
//
// kubespray builds the cluster with apiserver_loadbalancer_domain_name
// pointing at localhost, so kubeadm prints `kubeadm join 127.0.0.1:6443
// ...`. The DPU can't reach 127.0.0.1 of the host — we rewrite to
// publicAPIServerAddr (the routable host IP) so the join works.
func FetchJoinCommand(ctx context.Context, cp *ssh.Client, publicAPIServerAddr string) (*JoinCommand, error) {
	r := cp.Run(ctx, "sudo -n kubeadm token create --print-join-command 2>/dev/null")
	if !r.OK() {
		return nil, fmt.Errorf("kubeadm token create on control plane: exit=%d stderr=%s", r.ExitCode, strings.TrimSpace(r.Stderr))
	}
	line := strings.TrimSpace(r.Stdout)
	if idx := strings.LastIndex(line, "kubeadm join"); idx > 0 {
		line = line[idx:]
	}
	if !strings.HasPrefix(line, "kubeadm join") {
		return nil, fmt.Errorf("unexpected kubeadm output: %q", line)
	}
	if publicAPIServerAddr != "" {
		line = strings.ReplaceAll(line, "127.0.0.1:6443", publicAPIServerAddr+":6443")
		line = strings.ReplaceAll(line, "localhost:6443", publicAPIServerAddr+":6443")
	}
	return &JoinCommand{Raw: line}, nil
}

// InstallKubeBinaries installs kubelet/kubeadm/kubectl on the DPU OS
// matching the cluster's k8s minor version (e.g. 1.32). Idempotent —
// re-runs are cheap once apt has the repo.
func InstallKubeBinaries(ctx context.Context, dpu *ssh.Client, k8sMinor string) error {
	// pkgs.k8s.io organizes by minor (v1.32, v1.31, ...) — the patch
	// version comes from the apt resolver. We apt-mark hold so kubelet
	// doesn't drift on apt-upgrade.
	repoRel := "v" + k8sMinor
	script := strings.Join([]string{
		"set -e",
		"sudo -n apt-get update -qq",
		"sudo -n apt-get install -y -qq apt-transport-https ca-certificates curl gpg",
		"sudo -n install -m 0755 -d /etc/apt/keyrings",
		// idempotent: --batch --yes lets gpg overwrite an existing key.
		fmt.Sprintf("curl -fsSL https://pkgs.k8s.io/core:/stable:/%s/deb/Release.key | sudo -n gpg --batch --yes --dearmor -o /etc/apt/keyrings/kubernetes-apt-keyring.gpg", repoRel),
		fmt.Sprintf("echo 'deb [signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg] https://pkgs.k8s.io/core:/stable:/%s/deb/ /' | sudo -n tee /etc/apt/sources.list.d/kubernetes.list >/dev/null", repoRel),
		"sudo -n apt-get update -qq",
		"sudo -n apt-get install -y -qq kubelet kubeadm kubectl",
		"sudo -n apt-mark hold kubelet kubeadm kubectl",
		// containerd ships with the BlueField BSP but our bf.conf
		// cloud-init stops it as a "clean slate" for the operator.
		// Start it back up before kubeadm join, which preflight-checks
		// the CRI socket.
		"sudo -n systemctl enable --now containerd",
		// Stop kubelet so it doesn't crash-loop with no certs. kubeadm
		// join will start it at the end with the right config.
		"sudo -n systemctl disable --now kubelet || true",
		// Path bridge: kubespray puts the cluster CA at /etc/kubernetes/ssl/
		// but apt's kubeadm writes to /etc/kubernetes/pki/. The kubelet
		// config we'll receive from the cluster's kubeadm-config CM
		// references /etc/kubernetes/ssl/. Symlink so both resolve.
		"sudo -n mkdir -p /etc/kubernetes/pki",
		"sudo -n ln -sfn pki /etc/kubernetes/ssl",
	}, " && ")

	r := dpu.Run(ctx, script)
	if !r.OK() {
		return fmt.Errorf("install kube binaries on DPU: exit=%d\nstdout: %s\nstderr: %s",
			r.ExitCode, truncate(r.Stdout, 500), truncate(r.Stderr, 500))
	}
	return nil
}

// JoinDPU runs the kubeadm-supplied join command on the DPU, with our
// node name override. Containerd is the default CRI (set up by bf.conf).
func JoinDPU(ctx context.Context, dpu *ssh.Client, jc *JoinCommand, dpuHostname string) error {
	// Append --node-name so the DPU registers under the friendly name we
	// chose at flash time (worker1-bf3, worker2-bf3) rather than its
	// kernel hostname. Containerd default cri-socket also explicit.
	cmd := fmt.Sprintf("sudo -n %s --node-name %s --cri-socket unix:///run/containerd/containerd.sock 2>&1",
		jc.Raw, dpuHostname)
	// Long timeout — TLS bootstrap + initial pull can take a few minutes.
	joinCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	r := dpu.Run(joinCtx, cmd)
	if !r.OK() {
		return fmt.Errorf("kubeadm join on DPU: exit=%d\noutput: %s", r.ExitCode, truncate(r.Stdout+r.Stderr, 1000))
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
