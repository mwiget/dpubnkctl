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
	for _, want := range []string{"kube_version: 1.32.8", "kube_network_plugin: calico", "container_manager: containerd", "calico_mtu: 8900"} {
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
	} {
		if !strings.Contains(all, want) {
			t.Errorf("all.yml missing %q\n%s", want, all)
		}
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
