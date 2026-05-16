package cli

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/mwiget/dpubnkctl/internal/poc"
	"github.com/mwiget/dpubnkctl/internal/ssh"
)

// dpuSSHConfig builds an ssh.Config for connecting to a DPU through its
// host via ProxyJump. Used by every command that needs to SSH into a
// freshly-flashed or operating DPU — provision (post-flash verify),
// cluster join-dpus, deploy network (containerd restart), and destroy
// dpus.
//
// Topology: the DPU listens on its tmfifo_net0 (host-only link, IP from
// poc.yaml) and answers on port 22. Every DPU in every PoC answers at
// the same 192.168.100.2 — they're host-local, not routable — so the
// DPU side deliberately skips known_hosts (a shared file would collide
// the moment a second host's DPU connects). The trust boundary is the
// jumphost: its host key IS pinned via inventory/known_hosts on first
// successful connection.
func dpuSSHConfig(repo string, host *poc.Host, dpu *poc.DPU) (ssh.Config, error) {
	if dpu == nil || dpu.TmfifoIP == "" {
		return ssh.Config{}, fmt.Errorf("dpu has no tmfifo_ip")
	}
	dpuIP := strings.SplitN(dpu.TmfifoIP, "/", 2)[0]
	hostKey := host.SSH.KeyRef
	if !filepath.IsAbs(hostKey) {
		hostKey = filepath.Join(repo, hostKey)
	}
	known := filepath.Join(repo, "inventory", "known_hosts")
	return ssh.Config{
		Address: dpuIP,
		Port:    22,
		User:    "ubuntu",
		KeyPath: hostKey,
		Timeout: 30 * time.Second,
		Jumphost: &ssh.Config{
			Address:    host.SSH.Address,
			Port:       host.SSH.Port,
			User:       host.SSH.User,
			KeyPath:    hostKey,
			KnownHosts: known,
			Timeout:    30 * time.Second,
		},
	}, nil
}
