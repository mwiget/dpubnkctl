// Package cluster generates kubespray inventory + group_vars from a PoC
// and (in a follow-up) drives `kubespray cluster.yml` via Docker.
package cluster

import (
	"fmt"
	"net"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/mwiget/dpubnkctl/internal/poc"
	"github.com/mwiget/dpubnkctl/internal/version"
)

// stripCIDR returns the IP from a CIDR string ("10.10.41.66/24" → "10.10.41.66").
func stripCIDR(s string) (string, error) {
	ip, _, err := net.ParseCIDR(s)
	if err != nil {
		return "", err
	}
	return ip.String(), nil
}

// Plan describes the cluster topology dpubnkctl would build from poc.yaml.
// Validation happens in BuildPlan; rendering only runs on a valid Plan.
type Plan struct {
	ControlPlane []string // hostnames in kube_control_plane group
	Workers      []string // hostnames in kube_node group (control planes are also workers if Role == "both")
	Etcd         []string // hostnames in etcd group (odd-sized subset of ControlPlane)
	HostByName   map[string]*poc.Host
	Errors       []string // empty iff plan is valid
}

func (p Plan) Valid() bool { return len(p.Errors) == 0 }

// BuildPlan walks the PoC and decides which host belongs in which kubespray
// group. Host.Role values: "control-plane" | "worker" | "both".
//
// etcd quorum: kubespray puts every kube_control_plane in etcd by default,
// which breaks quorum at 2 control planes. We pick an odd subset:
//
//	1 control plane → 1 etcd (single point of failure, fine for PoC)
//	2 control planes → 1 etcd (NOT both — would deadlock at 1 failure)
//	3+ control planes → 3 etcd (proper HA)
func BuildPlan(p *poc.PoC) Plan {
	plan := Plan{HostByName: map[string]*poc.Host{}}

	if len(p.Hosts) == 0 {
		plan.Errors = append(plan.Errors, "no hosts in poc.yaml — run `dpubnkctl discover` first")
		return plan
	}

	for i := range p.Hosts {
		h := &p.Hosts[i]
		plan.HostByName[h.Name] = h

		if h.SSH.Address == "" {
			plan.Errors = append(plan.Errors, fmt.Sprintf("host %q: missing ssh.address", h.Name))
			continue
		}
		if h.SSH.User == "" {
			plan.Errors = append(plan.Errors, fmt.Sprintf("host %q: missing ssh.user", h.Name))
			continue
		}

		role := strings.ToLower(strings.TrimSpace(h.Role))
		switch role {
		case "control-plane":
			plan.ControlPlane = append(plan.ControlPlane, h.Name)
		case "worker":
			plan.Workers = append(plan.Workers, h.Name)
		case "both":
			plan.ControlPlane = append(plan.ControlPlane, h.Name)
			plan.Workers = append(plan.Workers, h.Name)
		case "":
			plan.Errors = append(plan.Errors, fmt.Sprintf("host %q: role is empty (set to control-plane | worker | both)", h.Name))
		default:
			plan.Errors = append(plan.Errors, fmt.Sprintf("host %q: unknown role %q", h.Name, h.Role))
		}
	}

	if len(plan.ControlPlane) == 0 {
		plan.Errors = append(plan.Errors, "no control-plane hosts (need at least one host with role: control-plane or both)")
	}
	if len(plan.Workers) == 0 {
		plan.Errors = append(plan.Errors, "no worker hosts (assign role: worker or both to at least one host)")
	}

	// Sort for stable output regardless of poc.yaml ordering.
	sort.Strings(plan.ControlPlane)
	sort.Strings(plan.Workers)

	plan.Etcd = pickEtcd(plan.ControlPlane)
	return plan
}

// KubesprayPinForCLI is exposed so the CLI layer can show the pin in
// `cluster plan` output without importing internal/version directly.
func KubesprayPinForCLI() string { return version.KubesprayVersion }

// pickEtcd returns an odd-sized quorum-safe subset of the control plane.
func pickEtcd(cp []string) []string {
	switch {
	case len(cp) == 0:
		return nil
	case len(cp) <= 2:
		return cp[:1] // single etcd avoids the 2-node deadlock
	case len(cp) >= 3:
		return cp[:3] // standard HA quorum
	}
	return cp
}

// Render produces all inventory files keyed by relative path. Caller
// writes them under <repo>/artifacts/kubespray-inventory/ (or wherever).
func Render(p *poc.PoC, plan Plan) (map[string]string, error) {
	if !plan.Valid() {
		return nil, fmt.Errorf("plan is invalid:\n  - %s", strings.Join(plan.Errors, "\n  - "))
	}
	out := map[string]string{}

	hostsYAML, err := renderHostsYAML(p, plan)
	if err != nil {
		return nil, err
	}
	out["hosts.yml"] = hostsYAML

	out[filepath.Join("group_vars", "all", "all.yml")] = renderGroupVarsAll(p)
	out[filepath.Join("group_vars", "k8s_cluster", "k8s-cluster.yml")] = renderGroupVarsK8sCluster(p)
	out["README.md"] = renderReadme(p, plan)
	return out, nil
}

// renderHostsYAML emits the kubespray inventory in YAML form. We use
// gopkg.in/yaml.v3 to get correct quoting and indentation.
//
// SSH keys are referenced via a per-inventory keys/ dir mounted at
// /inventory/keys inside the kubespray container — see runClusterUp,
// which copies each host's KeyRef there before invoking docker.
//
// Per-host `ip:` becomes kubelet --node-ip AND the etcd/apiserver
// advertised endpoint. When network.node_ip_role is set we pick the
// matching data-plane VLAN IP; otherwise fall back to ssh.address.
//
// We deliberately do NOT set kubespray's `access_ip` — that var
// overrides the externally-advertised endpoint for etcd/kubelet,
// which would mismatch the listen address (etcd binds to `ip`,
// advertises `access_ip`) and break the etcd healthcheck.
// `ansible_host` alone handles SSH reachability for ansible.
func renderHostsYAML(p *poc.PoC, plan Plan) (string, error) {
	role := p.Network.NodeIPRole
	hostsBlock := map[string]any{}
	for _, name := range allHostNames(plan) {
		h := plan.HostByName[name]
		nodeIP := h.SSH.Address
		if role != "" {
			v := h.VLANByRole(role)
			if v == nil {
				return "", fmt.Errorf("host %s: no data_plane.vlans entry with role=%q (network.node_ip_role)", name, role)
			}
			ip, err := stripCIDR(v.IP)
			if err != nil {
				return "", fmt.Errorf("host %s: bad IP %q on vlan %s: %w", name, v.IP, v.PortName(), err)
			}
			nodeIP = ip
		}
		entry := map[string]any{
			"ansible_host":                 h.SSH.Address,
			"ansible_user":                 h.SSH.User,
			"ip":                           nodeIP,
			"ansible_ssh_private_key_file": "/inventory/keys/" + name + ".pem",
		}
		if h.SSH.Port != 0 && h.SSH.Port != 22 {
			entry["ansible_port"] = h.SSH.Port
		}
		hostsBlock[name] = entry
	}

	cluster := map[string]any{
		"vars": map[string]any{
			"ansible_become":        true,
			"ansible_become_method": "sudo",
		},
		"hosts": hostsBlock,
		"children": map[string]any{
			"kube_control_plane": map[string]any{
				"hosts": namesToNullMap(plan.ControlPlane),
			},
			"kube_node": map[string]any{
				"hosts": namesToNullMap(plan.Workers),
			},
			"etcd": map[string]any{
				"hosts": namesToNullMap(plan.Etcd),
			},
			"k8s_cluster": map[string]any{
				"children": map[string]any{
					"kube_control_plane": nil,
					"kube_node":          nil,
				},
			},
		},
	}

	doc := map[string]any{"all": cluster}
	b, err := yaml.Marshal(doc)
	if err != nil {
		return "", err
	}
	header := fmt.Sprintf("# kubespray inventory — generated by dpubnkctl from %s\n# Do not edit by hand: re-run `dpubnkctl cluster plan` to regenerate.\n\n", poc.FileName)
	return header + string(b), nil
}

func renderGroupVarsAll(p *poc.PoC) string {
	var b strings.Builder
	b.WriteString(`# group_vars/all/all.yml — generated by dpubnkctl
# Cluster-wide ansible defaults.

ansible_become: true
ansible_become_method: sudo

# Skip TOFU host-key prompts inside the kubespray container; the operator
# trusts the lab/PoC network here. For production add a populated
# /inventory/known_hosts and flip this back on.
ansible_host_key_checking: false

# Disable upgrade prompts that block ansible runs.
upgrade_cluster_setup: false
`)
	if addr := p.Network.ClusterAPIServerAddress; addr != "" {
		// Point every kubelet/kube-proxy at a routable apiserver address
		// instead of the per-worker localhost nginx-proxy. Externally-
		// joined DPUs talk to this same address — without this they'd
		// inherit `127.0.0.1:6443` from the in-cluster kubeadm config and
		// fail to reach the apiserver.
		fmt.Fprintf(&b, `
# Apiserver reached via routable VIP/IP on the data-plane network.
# Disables kubespray's localhost-nginx-proxy convention so DPUs joined
# externally can reach the apiserver at the same address every node uses.
loadbalancer_apiserver_localhost: false
loadbalancer_apiserver:
  address: %[1]s
  port: 6443
apiserver_loadbalancer_domain_name: %[1]s
supplementary_addresses_in_ssl_keys:
  - %[1]s
`, addr)
		// Also include each host's management/SSH address in the apiserver
		// cert SANs so the operator's local kubectl works against the
		// mgmt path (data-plane usually isn't routable from outside the
		// lab — see AGENTS.md #23). Without this, kubectl from the
		// operator host needs --insecure-skip-tls-verify.
		for i := range p.Hosts {
			if a := p.Hosts[i].SSH.Address; a != "" && a != addr {
				fmt.Fprintf(&b, "  - %s\n", a)
			}
		}
	}
	return b.String()
}

func renderGroupVarsK8sCluster(p *poc.PoC) string {
	return fmt.Sprintf(`# group_vars/k8s_cluster/k8s-cluster.yml — generated by dpubnkctl
# Pinned to BNK %[1]s requirements (kubespray %[2]s).

kube_version: %[3]s

# CNI / networking
kube_network_plugin: calico
kube_pods_subnet: 10.233.64.0/18
kube_service_addresses: 10.233.0.0/18
calico_mtu: %[4]d

# kube_proxy_mode: iptables (not the kubespray default 'ipvs').
# Kubespray v2.28.1 + Ubuntu 22.04 kernel 5.15+ trips on a broken
# modprobe loop for the no-longer-existent nf_conntrack_ipv4 module
# when kube_proxy_mode == ipvs. iptables sidesteps the bug entirely and
# is the kubeadm default. Override here if your kernel pre-loads the
# legacy modules.
kube_proxy_mode: iptables

# Container runtime
container_manager: containerd
containerd_version: %[5]s
runc_version: %[6]s
pause_image_tag: "%[7]s"

# Cluster ergonomics — allow workloads on control-plane nodes
# (single-node and 2-node PoCs need this; multi-node should remove it).
kubelet_load_modules: true
`,
		p.Metadata.BNKVersion,
		version.KubesprayVersion,
		version.K8sVersionPinned,
		p.Network.PodMTU,
		p.Versions.Containerd,
		p.Versions.Runc,
		p.Versions.PauseTag,
	)
}

func renderReadme(p *poc.PoC, plan Plan) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# kubespray-inventory for %s\n\n", p.Metadata.Name)
	fmt.Fprintf(&b, "Generated by `dpubnkctl cluster plan` from `%s`. Do not hand-edit;\n", poc.FileName)
	fmt.Fprintln(&b, "re-run `cluster plan` after changing `poc.yaml`.")
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "Targets BNK %s using kubespray %s (k8s %s).\n\n", p.Metadata.BNKVersion, version.KubesprayVersion, version.K8sVersionPinned)
	fmt.Fprintln(&b, "## Group membership")
	fmt.Fprintf(&b, "- `kube_control_plane` (%d): %s\n", len(plan.ControlPlane), strings.Join(plan.ControlPlane, ", "))
	fmt.Fprintf(&b, "- `kube_node` (%d):          %s\n", len(plan.Workers), strings.Join(plan.Workers, ", "))
	fmt.Fprintf(&b, "- `etcd` (%d):               %s\n", len(plan.Etcd), strings.Join(plan.Etcd, ", "))
	fmt.Fprintln(&b, "")
	if len(plan.ControlPlane) == 2 {
		fmt.Fprintln(&b, "> NOTE: 2 control planes is not HA-safe. dpubnkctl picked 1 etcd to keep quorum;")
		fmt.Fprintln(&b, "> add a 3rd control-plane host (or accept the single-etcd risk) for production.")
	}
	fmt.Fprintln(&b, "")
	fmt.Fprintln(&b, "## Manual invocation (for reference)")
	fmt.Fprintln(&b, "```")
	fmt.Fprintf(&b, "docker run --rm -it \\\n")
	fmt.Fprintf(&b, "  -v $(pwd):/inventory \\\n")
	fmt.Fprintf(&b, "  -v $HOME/.ssh:/root/.ssh:ro \\\n")
	fmt.Fprintf(&b, "  %s \\\n", version.KubesprayImage)
	fmt.Fprintln(&b, "  ansible-playbook -i /inventory/hosts.yml cluster.yml")
	fmt.Fprintln(&b, "```")
	fmt.Fprintln(&b, "")
	fmt.Fprintln(&b, "Or use `dpubnkctl cluster up` to do this end-to-end.")
	return b.String()
}

func allHostNames(plan Plan) []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range plan.ControlPlane {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	for _, n := range plan.Workers {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// namesToNullMap returns the "host: ~" idiom kubespray uses to express
// group membership without per-host vars.
func namesToNullMap(names []string) map[string]any {
	out := make(map[string]any, len(names))
	for _, n := range names {
		out[n] = nil
	}
	return out
}
