package provision

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mwiget/dpubnkctl/internal/poc"
)

// fixturePoC returns a fully-populated PoC + host + DPU pair so the
// renderer has every required field present.
func fixturePoC(t *testing.T, lag bool) (*poc.PoC, *poc.Host, *poc.DPU, string) {
	t.Helper()
	repo := t.TempDir()
	hashPath := filepath.Join(repo, "keys", "dpu_password.hash")
	if err := os.MkdirAll(filepath.Dir(hashPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hashPath, []byte("$1$abcd1234$5678901234567890123456\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	dpu := poc.DPU{
		PCI:      "00:10.0",
		Mode:     "dpu",
		LAG:      lag,
		Hostname: "worker1-bf3",
		TmfifoIP: "192.168.100.2/30",
		VLANs: []poc.DPUVLAN{
			{Name: "external0", Tag: 40, IP: "10.10.40.5/24", DefaultGateway: "10.10.40.1", Uplink: "p0"},
			{Name: "internal0", Tag: 41, IP: "10.10.41.5/24", Uplink: "p1"},
		},
	}
	keyPath := filepath.Join(repo, "keys", "id_test")
	pubPath := keyPath + ".pub"
	if err := os.WriteFile(keyPath, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pubPath, []byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAATEST test@op\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := poc.Host{
		Name: "worker1",
		SSH:  poc.SSH{Address: "192.168.68.66", Port: 22, User: "ubuntu", KeyRef: keyPath},
		DPUs: []poc.DPU{dpu},
	}
	p := poc.New("test-poc")
	p.Network.DPUMTU = 9000
	p.Provisioning.DPUDNS = []string{"8.8.8.8"}
	p.Provisioning.DPUDNSDomains = []string{"lab.example.com"}
	p.Provisioning.DPUNTP = []string{"pool.ntp.org"}
	p.Hosts = []poc.Host{h}
	return p, &p.Hosts[0], &p.Hosts[0].DPUs[0], repo
}

func TestRender_LAG(t *testing.T) {
	p, h, d, repo := fixturePoC(t, true)
	out, err := Render(p, h, d, repo)
	if err != nil {
		t.Fatal(err)
	}
	wants := []string{
		"ubuntu_PASSWORD='$1$",                  // password hash substituted
		"MTU=9000",                               // MTU substituted
		`local hname="worker1-bf3"`,             // hostname substituted
		"- 192.168.100.2/30",                    // tmfifo IP
		"DNS=8.8.8.8",                            // DNS
		"NTP=pool.ntp.org",                       // NTP
		"OVS_BRIDGE1=\"br-lag\"",                // LAG bridge
		"add-port br-lag external0",             // VLAN goes onto br-lag
		"tag=40",                                 // VLAN tag substituted
		"ip route replace default via 10.10.40.1", // default gateway present
		"LAG_RESOURCE_ALLOCATION=1",              // bfb_post_install
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAATEST", // operator pubkey installed
		"chmod 600 /mnt/home/ubuntu/.ssh/authorized_keys", // perms set
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("LAG render missing %q", w)
		}
	}
	// Should NOT contain non-LAG bridge name.
	if strings.Contains(out, "sf-external") {
		t.Errorf("LAG render leaked non-LAG bridge name")
	}
}

func TestRender_NonLAG_BridgePerUplink(t *testing.T) {
	p, h, d, repo := fixturePoC(t, false)
	out, err := Render(p, h, d, repo)
	if err != nil {
		t.Fatal(err)
	}
	wants := []string{
		"OVS_BRIDGE1=\"sf-external\"",
		"OVS_BRIDGE2=\"sf-internal\"",
		"add-port sf-external external0", // VLAN external0 (uplink p0) goes to sf-external
		"add-port sf-internal internal0", // VLAN internal0 (uplink p1) goes to sf-internal
		"LAG_RESOURCE_ALLOCATION=DEVICE_DEFAULT",
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("non-LAG render missing %q", w)
		}
	}
	if strings.Contains(out, "br-lag") {
		t.Errorf("non-LAG render leaked LAG bridge name")
	}
}

func TestRender_MissingFields(t *testing.T) {
	p, h, d, repo := fixturePoC(t, true)
	d.Hostname = "" // remove a required field
	d.TmfifoIP = ""
	_, err := Render(p, h, d, repo)
	if err == nil {
		t.Fatal("expected error for missing required fields")
	}
	if !strings.Contains(err.Error(), "hostname") || !strings.Contains(err.Error(), "tmfifo_ip") {
		t.Errorf("error should list both missing fields; got: %v", err)
	}
}

func TestRender_BadPasswordHash(t *testing.T) {
	p, h, d, repo := fixturePoC(t, true)
	hashPath := filepath.Join(repo, "keys", "dpu_password.hash")
	_ = os.WriteFile(hashPath, []byte("plaintext-not-a-hash"), 0o600)
	_, err := Render(p, h, d, repo)
	if err == nil {
		t.Fatal("expected error for non-crypt-format hash")
	}
	if !strings.Contains(err.Error(), "$1$") {
		t.Errorf("error should mention required hash format; got: %v", err)
	}
}

func TestRender_NonLAG_RequiresUplink(t *testing.T) {
	p, h, d, repo := fixturePoC(t, false)
	d.VLANs[0].Uplink = "" // strip uplink
	_, err := Render(p, h, d, repo)
	if err == nil {
		t.Fatal("expected error for VLAN with no uplink in non-LAG mode")
	}
}
