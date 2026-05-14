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
	// version comes from the apt resolver. We apt-mark hold AFTER install
	// so kubelet doesn't drift on apt-upgrade. We apt-mark UNHOLD first
	// because the BlueField BSP pre-holds kubelet at 1.30.14; without
	// the unhold, `apt-get install` is a no-op against that pinned
	// version and the DPU joins with a kubelet too old for the cluster.
	// (AGENTS.md #10.)
	repoRel := "v" + k8sMinor
	script := strings.Join([]string{
		"set -e",
		// Clear the BSP-applied hold before adding the upstream repo and
		// installing. `|| true` because on a fresh DPU the packages may
		// not be present at all (nothing to unhold), and apt-mark errors
		// when given unknown packages.
		"sudo -n apt-mark unhold kubelet kubeadm kubectl || true",
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

// RestartContainerd runs `systemctl restart containerd` on the given
// SSH client. Used after kubeadm join + post-CNI install to flush
// containerd's CRI cache (the "no CNI" status sticks until restart;
// see AGENTS.md #5).
func RestartContainerd(ctx context.Context, c *ssh.Client) error {
	r := c.Run(ctx, "sudo -n systemctl restart containerd")
	if !r.OK() {
		return fmt.Errorf("restart containerd: exit=%d %s", r.ExitCode, strings.TrimSpace(r.Stderr+r.Stdout))
	}
	return nil
}

// AlreadyJoined returns true if the DPU has a usable kubelet.conf —
// the file kubeadm join writes once TLS bootstrap succeeds. Used by
// JoinDPU to skip the join when it would otherwise fail-loud with
// "/etc/kubernetes/kubelet.conf already exists". A partial-success
// retry of `cluster join-dpus` against an already-joined DPU should
// be a no-op, not an error.
func AlreadyJoined(ctx context.Context, dpu *ssh.Client) bool {
	r := dpu.Run(ctx, "test -s /etc/kubernetes/kubelet.conf")
	return r.OK()
}

// JoinDPU runs the kubeadm-supplied join command on the DPU, with our
// node name override. Containerd is the default CRI (set up by bf.conf).
// If the DPU already has /etc/kubernetes/kubelet.conf, JoinDPU returns
// nil immediately — the DPU was joined on an earlier pass and a retry
// must be idempotent.
//
// nodeIP, when non-empty, is written to /etc/default/kubelet as
// KUBELET_EXTRA_ARGS=--node-ip=<ip> BEFORE kubeadm join runs. kubeadm
// join then starts kubelet via systemd, which sources /etc/default/
// kubelet — so kubelet registers with that --node-ip. Required when
// the cluster's east-west fabric is on a different subnet than the
// DPU's management (oob_net0) IP. (We can't pass --node-ip to kubeadm
// join directly — it's a kubelet flag, not a kubeadm one.)
//
// joined reports whether the join actually ran (true) or was skipped
// because the DPU was already a member (false). Callers use this to
// decide whether to label/taint anew — the label/taint themselves are
// idempotent via `kubectl label --overwrite` so it's mostly informational.
func JoinDPU(ctx context.Context, dpu *ssh.Client, jc *JoinCommand, dpuHostname, nodeIP string) (joined bool, err error) {
	if AlreadyJoined(ctx, dpu) {
		// Make sure kubelet is enabled+running even on the skip path —
		// InstallKubeBinaries always disables kubelet to avoid a cert-less
		// crash loop, but on a retry that previously got past install we
		// must re-enable so the DPU stays in the cluster.
		_ = dpu.Run(ctx, "sudo -n systemctl enable --now kubelet")
		return false, nil
	}
	if nodeIP != "" {
		// Drop a small file kubelet's systemd unit reads via EnvironmentFile=
		// (kubespray + kubeadm both wire this in). Single line, idempotent
		// — re-running join overwrites it with the same content.
		env := fmt.Sprintf(`echo 'KUBELET_EXTRA_ARGS=--node-ip=%s' | sudo -n tee /etc/default/kubelet >/dev/null`, nodeIP)
		if r := dpu.Run(ctx, env); !r.OK() {
			return false, fmt.Errorf("write /etc/default/kubelet: exit=%d %s", r.ExitCode, strings.TrimSpace(r.Stderr+r.Stdout))
		}
	}
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
		// Failure path: kubelet was disabled by InstallKubeBinaries, restore
		// to running state so the DPU isn't left as an inert "kubelet
		// inactive (dead)" — the operator can't tell from outside whether
		// the join half-succeeded. Best-effort.
		_ = dpu.Run(ctx, "sudo -n systemctl enable --now kubelet")
		return false, fmt.Errorf("kubeadm join on DPU: exit=%d\noutput: %s", r.ExitCode, truncate(r.Stdout+r.Stderr, 1000))
	}
	return true, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
