package cluster

import (
	"fmt"
	"strings"
	"testing"

	"github.com/mwiget/dpubnkctl/internal/poc"
)

func fixture(roles ...string) *poc.PoC {
	p := poc.New("test")
	p.Network.PodMTU = 8900
	for i, role := range roles {
		p.Hosts = append(p.Hosts, poc.Host{
			Name: fmt.Sprintf("h%d", i+1),
			Role: role,
			SSH:  poc.SSH{Address: fmt.Sprintf("10.0.0.%d", 10+i), Port: 22, User: "ubuntu"},
		})
	}
	return p
}

func TestBuildPlan_BothRole(t *testing.T) {
	p := fixture("both", "both")
	plan := BuildPlan(p)
	if !plan.Valid() {
		t.Fatalf("expected valid plan, got errors: %v", plan.Errors)
	}
	if len(plan.ControlPlane) != 2 || len(plan.Workers) != 2 {
		t.Errorf("control=%v workers=%v", plan.ControlPlane, plan.Workers)
	}
	if len(plan.Etcd) != 1 {
		t.Errorf("expected 1 etcd for 2 control planes (quorum), got %v", plan.Etcd)
	}
}

func TestBuildPlan_SeparateRoles(t *testing.T) {
	p := fixture("control-plane", "control-plane", "control-plane", "worker")
	plan := BuildPlan(p)
	if !plan.Valid() {
		t.Fatalf("errors: %v", plan.Errors)
	}
	if len(plan.ControlPlane) != 3 || len(plan.Workers) != 1 {
		t.Errorf("control=%v workers=%v", plan.ControlPlane, plan.Workers)
	}
	if len(plan.Etcd) != 3 {
		t.Errorf("expected 3 etcd for 3+ control planes, got %v", plan.Etcd)
	}
}

func TestBuildPlan_NoControlPlane(t *testing.T) {
	p := fixture("worker", "worker")
	plan := BuildPlan(p)
	if plan.Valid() {
		t.Fatal("expected invalid plan (no control plane)")
	}
	found := false
	for _, e := range plan.Errors {
		if strings.Contains(e, "control-plane") {
			found = true
		}
	}
	if !found {
		t.Errorf("error list should mention missing control plane: %v", plan.Errors)
	}
}

func TestBuildPlan_NoWorker(t *testing.T) {
	p := fixture("control-plane")
	plan := BuildPlan(p)
	if plan.Valid() {
		t.Fatal("expected invalid plan (no worker)")
	}
}

func TestBuildPlan_EmptyRole(t *testing.T) {
	p := fixture("")
	plan := BuildPlan(p)
	if plan.Valid() {
		t.Fatal("expected invalid plan (empty role)")
	}
}

func TestBuildPlan_UnknownRole(t *testing.T) {
	p := fixture("master") // legacy term — not accepted
	plan := BuildPlan(p)
	if plan.Valid() {
		t.Fatal("expected invalid plan (unknown role)")
	}
}

func TestRender_Files(t *testing.T) {
	p := fixture("both", "both")
	plan := BuildPlan(p)
	files, err := Render(p, plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"hosts.yml", "group_vars/all/all.yml", "group_vars/k8s_cluster/k8s-cluster.yml", "README.md"} {
		if _, ok := files[want]; !ok {
			t.Errorf("missing file %q in render", want)
		}
	}
	hosts := files["hosts.yml"]
	for _, want := range []string{"kube_control_plane", "kube_node", "etcd", "k8s_cluster", "h1:", "h2:", "ansible_become: true"} {
		if !strings.Contains(hosts, want) {
			t.Errorf("hosts.yml missing %q\n%s", want, hosts)
		}
	}
	cluster := files["group_vars/k8s_cluster/k8s-cluster.yml"]
	for _, want := range []string{"kube_version: 1.30.14", "kube_network_plugin: calico", "container_manager: containerd", "calico_mtu: 8900"} {
		if !strings.Contains(cluster, want) {
			t.Errorf("k8s-cluster.yml missing %q", want)
		}
	}
}

func TestRender_DataPlaneNodeIP(t *testing.T) {
	p := fixture("both", "both")
	p.Network.NodeIPRole = "internal"
	p.Network.ClusterAPIServerAddress = "10.10.41.66"
	for i := range p.Hosts {
		p.Hosts[i].DataPlane = &poc.HostDataPlane{
			ParentIface: "ens16f0np0",
			VLANs: []poc.HostDataPlaneVLAN{
				{Role: "external", Tag: 40, IP: fmt.Sprintf("10.10.40.%d/24", 66+i)},
				{Role: "internal", Tag: 41, IP: fmt.Sprintf("10.10.41.%d/24", 66+i)},
			},
		}
	}
	plan := BuildPlan(p)
	files, err := Render(p, plan)
	if err != nil {
		t.Fatal(err)
	}
	hosts := files["hosts.yml"]
	for _, want := range []string{
		"ip: 10.10.41.66",       // h1 nodeIP from internal/41
		"ip: 10.10.41.67",       // h2 nodeIP from internal/41
		"ansible_host: 10.0.0.10", // h1 keeps mgmt as ansible_host
		"ansible_host: 10.0.0.11", // h2 keeps mgmt as ansible_host
	} {
		if !strings.Contains(hosts, want) {
			t.Errorf("hosts.yml missing %q\n%s", want, hosts)
		}
	}
	// Must NOT emit access_ip — that would override etcd/apiserver
	// advertised endpoints and break the etcd healthcheck.
	if strings.Contains(hosts, "access_ip:") {
		t.Errorf("hosts.yml should NOT contain access_ip (kubespray would mis-advertise etcd):\n%s", hosts)
	}
	all := files["group_vars/all/all.yml"]
	for _, want := range []string{
		"loadbalancer_apiserver_localhost: false",
		"address: 10.10.41.66",
		"port: 6443",
		"apiserver_loadbalancer_domain_name: 10.10.41.66",
		"supplementary_addresses_in_ssl_keys",
		"- 10.10.41.66",
		"- 10.0.0.10", // h1 mgmt addr also added to cert SAN
		"- 10.0.0.11", // h2 mgmt addr also added to cert SAN
	} {
		if !strings.Contains(all, want) {
			t.Errorf("all.yml missing %q\n%s", want, all)
		}
	}
}

func TestRender_RshimSANs(t *testing.T) {
	// Under rshim the host keeps its mgmt node-ip (no loadbalancer
	// override), and the apiserver cert SANs carry both the mgmt IP and
	// the host's tmfifo IP.
	p := fixture("both")
	p.Network.JoinTransport = poc.JoinTransportRshim
	p.Hosts[0].TmfifoIP = "192.168.100.1/30"
	plan := BuildPlan(p)
	files, err := Render(p, plan)
	if err != nil {
		t.Fatal(err)
	}
	all := files["group_vars/all/all.yml"]
	for _, want := range []string{
		"supplementary_addresses_in_ssl_keys",
		"- 10.0.0.10",      // mgmt IP (kubectl-from-host path)
		"- 192.168.100.1",  // tmfifo IP (DPU join path)
	} {
		if !strings.Contains(all, want) {
			t.Errorf("all.yml missing %q\n%s", want, all)
		}
	}
	// rshim must NOT force the loadbalancer override — host stays on mgmt.
	if strings.Contains(all, "loadbalancer_apiserver_localhost") {
		t.Errorf("rshim should not set loadbalancer_apiserver override\n%s", all)
	}
	// host node-ip stays mgmt (no data-plane role lookup).
	if !strings.Contains(files["hosts.yml"], "ip: 10.0.0.10") {
		t.Errorf("rshim host node-ip should be mgmt 10.0.0.10\n%s", files["hosts.yml"])
	}
}

func TestRender_NoJumphost_NoSSHCommonArgs(t *testing.T) {
	// Without a jumphost, hosts.yml must NOT emit ansible_ssh_common_args
	// and renderInventorySSHConfig must produce an empty string.
	p := fixture("both")
	plan := BuildPlan(p)
	files, err := Render(p, plan)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(files["hosts.yml"], "ansible_ssh_common_args") {
		t.Errorf("hosts.yml should not emit ansible_ssh_common_args without a jumphost:\n%s", files["hosts.yml"])
	}
	if got := renderInventorySSHConfig(plan); got != "" {
		t.Errorf("renderInventorySSHConfig should be empty without a jumphost; got:\n%s", got)
	}
}

func TestRender_JumphostSharedKey(t *testing.T) {
	// Jumphost set, but JumphostKeyRef unset → re-use the target's key
	// for both hops. ssh_config references the same /inventory/keys/<host>.pem.
	p := fixture("both")
	p.Hosts[0].SSH.Jumphost = "10.196.23.100"
	p.Hosts[0].SSH.KeyRef = "keys/mgx-21.pem"
	plan := BuildPlan(p)
	files, err := Render(p, plan)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(files["hosts.yml"], "ansible_ssh_common_args: -F /inventory/ssh_config") {
		t.Errorf("hosts.yml should include ansible_ssh_common_args when jumphost is set:\n%s", files["hosts.yml"])
	}
	sshCfg := renderInventorySSHConfig(plan)
	for _, want := range []string{
		"Host h1-jump",
		"HostName 10.196.23.100",
		"IdentityFile /inventory/keys/h1.pem", // shared with target
		"ProxyJump h1-jump",
		"Host 10.0.0.10",
	} {
		if !strings.Contains(sshCfg, want) {
			t.Errorf("ssh_config missing %q\n%s", want, sshCfg)
		}
	}
	if strings.Contains(sshCfg, "h1-jump.pem") {
		t.Errorf("ssh_config should NOT reference a separate jump key when JumphostKeyRef is unset:\n%s", sshCfg)
	}
}

func TestRender_JumphostSeparateKey(t *testing.T) {
	// Jumphost + JumphostUser + JumphostKeyRef all set → separate stanza
	// uses the jump key + jump user, target stanza keeps target key + user.
	p := fixture("both")
	p.Hosts[0].SSH.Jumphost = "10.196.23.100"
	p.Hosts[0].SSH.JumphostUser = "operator"
	p.Hosts[0].SSH.JumphostKeyRef = "keys/workstation.ed25519"
	p.Hosts[0].SSH.User = "ubuntu"
	p.Hosts[0].SSH.KeyRef = "keys/jumper.ed25519"
	plan := BuildPlan(p)
	sshCfg := renderInventorySSHConfig(plan)
	for _, want := range []string{
		"Host h1-jump",
		"User operator",                            // jumphost user override
		"IdentityFile /inventory/keys/h1-jump.pem", // separate jump key
		"Host 10.0.0.10",
		"User ubuntu",                         // target keeps own user
		"IdentityFile /inventory/keys/h1.pem", // target keeps own key
	} {
		if !strings.Contains(sshCfg, want) {
			t.Errorf("ssh_config missing %q\n%s", want, sshCfg)
		}
	}
}

func TestLocalizeKubeconfig_Insecure(t *testing.T) {
	// kubespray's apiserver_loadbalancer_domain_name path: server points
	// at the data-plane IP that the operator can't route to.
	raw := `apiVersion: v1
clusters:
- cluster:
    certificate-authority-data: LS0tLS1CRUdJTi==
    server: https://10.10.41.66:6443
  name: cluster.local
`
	got := LocalizeKubeconfig(raw, "192.168.68.66", true)
	for _, want := range []string{
		"server: https://192.168.68.66:6443",
		"insecure-skip-tls-verify: true",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in localized kubeconfig:\n%s", want, got)
		}
	}
	if strings.Contains(got, "10.10.41.66:6443") {
		t.Errorf("data-plane URL not rewritten:\n%s", got)
	}
	if strings.Contains(got, "certificate-authority-data:") {
		t.Errorf("CA data should be stripped (replaced by insecure-skip-tls-verify):\n%s", got)
	}

	// And the legacy 127.0.0.1 path still works.
	raw2 := `clusters:
- cluster:
    server: https://127.0.0.1:6443
`
	got2 := LocalizeKubeconfig(raw2, "192.168.68.66", true)
	if !strings.Contains(got2, "server: https://192.168.68.66:6443") {
		t.Errorf("127.0.0.1 path not rewritten:\n%s", got2)
	}
}

func TestLocalizeKubeconfig_Secure(t *testing.T) {
	// With insecure=false: rewrite server URL but keep CA data so kubectl
	// validates the apiserver cert. Used when the kubespray inventory
	// added mgmt addresses to supplementary_addresses_in_ssl_keys.
	raw := `apiVersion: v1
clusters:
- cluster:
    certificate-authority-data: LS0tLS1CRUdJTi==
    server: https://10.10.41.66:6443
  name: cluster.local
`
	got := LocalizeKubeconfig(raw, "192.168.68.66", false)
	if !strings.Contains(got, "server: https://192.168.68.66:6443") {
		t.Errorf("server URL not rewritten:\n%s", got)
	}
	if !strings.Contains(got, "certificate-authority-data: LS0tLS1CRUdJTi==") {
		t.Errorf("CA data should be preserved in secure mode:\n%s", got)
	}
	if strings.Contains(got, "insecure-skip-tls-verify") {
		t.Errorf("insecure-skip-tls-verify must NOT be set in secure mode:\n%s", got)
	}
}

func TestRender_NodeIPRoleMissing(t *testing.T) {
	p := fixture("both")
	p.Network.NodeIPRole = "internal"
	// h1 has NO data_plane block — render must error clearly.
	plan := BuildPlan(p)
	if _, err := Render(p, plan); err == nil {
		t.Fatal("expected error when node_ip_role set but host lacks data_plane")
	} else if !strings.Contains(err.Error(), `role="internal"`) {
		t.Errorf("error should mention the missing role: %v", err)
	}
}

func TestRender_RefusesInvalidPlan(t *testing.T) {
	p := fixture("worker", "worker") // no control plane
	plan := BuildPlan(p)
	_, err := Render(p, plan)
	if err == nil {
		t.Fatal("expected error for invalid plan")
	}
}
