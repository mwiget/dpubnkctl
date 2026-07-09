package cli

import (
	"strings"
	"testing"

	"github.com/mwiget/dpubnkctl/internal/cluster"
	"github.com/mwiget/dpubnkctl/internal/poc"
)

func TestResolveDPUNodeIP_Rshim(t *testing.T) {
	d := &poc.DPU{Hostname: "dpu1", TmfifoIP: "192.168.100.2/30"}
	got, err := resolveDPUNodeIP(d, poc.Network{JoinTransport: poc.JoinTransportRshim})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != "192.168.100.2" {
		t.Errorf("rshim node-ip = %q, want bare tmfifo IP 192.168.100.2", got)
	}
}

func TestResolveDPUNodeIP_VlanRoleUnset(t *testing.T) {
	d := &poc.DPU{Hostname: "dpu1", TmfifoIP: "192.168.100.2/30"}
	got, err := resolveDPUNodeIP(d, poc.Network{})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != "" {
		t.Errorf("vlan + no node_ip_role should auto-detect (empty), got %q", got)
	}
}

func TestResolveDPUNodeIP_VlanRole(t *testing.T) {
	d := &poc.DPU{
		Hostname: "dpu1",
		TmfifoIP: "192.168.100.2/30",
		VLANs:    []poc.DPUVLAN{{Role: "internal", Tag: 41, IP: "10.10.41.5/24", Uplink: "p1"}},
	}
	got, err := resolveDPUNodeIP(d, poc.Network{NodeIPRole: "internal"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != "10.10.41.5" {
		t.Errorf("vlan node-ip = %q, want 10.10.41.5", got)
	}
}

func TestKubeconfigNeedsInsecure(t *testing.T) {
	cases := []struct {
		name string
		p    *poc.PoC
		want bool
	}{
		{"vlan, no apiserver addr → insecure", &poc.PoC{Network: poc.Network{}}, true},
		{"vlan, apiserver addr set → secure", &poc.PoC{Network: poc.Network{ClusterAPIServerAddress: "10.10.41.66"}}, false},
		{"rshim → secure (SAN covers mgmt)", &poc.PoC{Network: poc.Network{JoinTransport: poc.JoinTransportRshim}}, false},
	}
	for _, c := range cases {
		if got := kubeconfigNeedsInsecure(c.p); got != c.want {
			t.Errorf("%s: kubeconfigNeedsInsecure = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestRshimKubeconfigIsSecure asserts the end-to-end result of the rshim
// guard: because needInsecure is false, cluster up localizes admin.conf
// against the host mgmt address with insecure=false — server rewritten to
// the mgmt IP, CA data preserved, no insecure-skip-tls-verify. This is the
// forge-usable kubeconfig deploy/join-dpus/bnk-forge all rely on.
func TestRshimKubeconfigIsSecure(t *testing.T) {
	p := &poc.PoC{Network: poc.Network{JoinTransport: poc.JoinTransportRshim}}
	insecure := kubeconfigNeedsInsecure(p) // false for rshim
	if insecure {
		t.Fatalf("rshim must not need an insecure kubeconfig")
	}
	adminConf := `apiVersion: v1
clusters:
- cluster:
    certificate-authority-data: LS0tLS1CRUdJTi==
    server: https://127.0.0.1:6443
  name: cluster.local
`
	got := cluster.LocalizeKubeconfig(adminConf, "192.0.2.10", insecure)
	if !strings.Contains(got, "server: https://192.0.2.10:6443") {
		t.Errorf("rshim kubeconfig server not rewritten to mgmt IP:\n%s", got)
	}
	if !strings.Contains(got, "certificate-authority-data: LS0tLS1CRUdJTi==") {
		t.Errorf("rshim kubeconfig should keep CA data (TLS intact):\n%s", got)
	}
	if strings.Contains(got, "insecure-skip-tls-verify") {
		t.Errorf("rshim kubeconfig must not disable TLS verification:\n%s", got)
	}
}

func TestBareIP(t *testing.T) {
	cases := map[string]string{
		"192.168.100.1/30": "192.168.100.1",
		"10.0.0.5":         "10.0.0.5",
		"":                 "",
		"not-an-ip":        "",
	}
	for in, want := range cases {
		if got := bareIP(in); got != want {
			t.Errorf("bareIP(%q) = %q, want %q", in, got, want)
		}
	}
}
