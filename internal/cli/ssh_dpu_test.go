package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/mwiget/dpubnkctl/internal/poc"
)

func dpuTestRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	for _, rel := range []string{"keys/host.pem", "keys/workstation.pem"} {
		full := filepath.Join(repo, rel)
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return repo
}

// TestDPUSSHConfig_TwoHop pins the legacy two-hop behaviour: operator
// has a direct route to the host, host ProxyJumps to the DPU. No bastion
// in front of the host.
func TestDPUSSHConfig_TwoHop(t *testing.T) {
	repo := dpuTestRepo(t)
	h := &poc.Host{
		SSH: poc.SSH{Address: "10.10.1.10", Port: 22, User: "ubuntu", KeyRef: "keys/host.pem"},
	}
	d := &poc.DPU{TmfifoIP: "192.168.100.2/30"}
	cfg, err := dpuSSHConfig(repo, h, d)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Address != "192.168.100.2" {
		t.Errorf("DPU target address = %q, want 192.168.100.2", cfg.Address)
	}
	if cfg.Jumphost == nil {
		t.Fatal("expected Jumphost (host) set")
	}
	if cfg.Jumphost.Address != "10.10.1.10" {
		t.Errorf("host-hop address = %q, want 10.10.1.10", cfg.Jumphost.Address)
	}
	if cfg.Jumphost.Jumphost != nil {
		t.Errorf("no bastion configured on host — expected single-jumphost chain, got nested:\n  %+v", cfg.Jumphost.Jumphost)
	}
}

// TestDPUSSHConfig_ThreeHopSharedKey covers the transatlantic operator
// shape where the host itself sits behind a bastion. ProxyJump chains:
// operator → bastion → host → DPU. The host's KeyRef is reused for
// every hop when JumphostKeyRef is unset.
func TestDPUSSHConfig_ThreeHopSharedKey(t *testing.T) {
	repo := dpuTestRepo(t)
	h := &poc.Host{
		SSH: poc.SSH{
			Address:  "198.18.0.21",
			Port:     22,
			User:     "ubuntu",
			KeyRef:   "keys/host.pem",
			Jumphost: "10.196.23.100",
		},
	}
	d := &poc.DPU{TmfifoIP: "192.168.100.2/30"}
	cfg, err := dpuSSHConfig(repo, h, d)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Jumphost == nil || cfg.Jumphost.Jumphost == nil {
		t.Fatalf("expected DPU → host → bastion nested chain; got:\n  %+v", cfg)
	}
	if cfg.Jumphost.Jumphost.Address != "10.196.23.100" {
		t.Errorf("bastion address = %q, want 10.196.23.100", cfg.Jumphost.Jumphost.Address)
	}
	if cfg.Jumphost.Jumphost.User != "ubuntu" {
		t.Errorf("bastion user defaulted = %q, want ubuntu (= host.SSH.User)", cfg.Jumphost.Jumphost.User)
	}
	// All three hops should use the same key when JumphostKeyRef is unset.
	want := filepath.Join(repo, "keys/host.pem")
	for _, hop := range []struct {
		name string
		path string
	}{
		{"target (DPU)", cfg.KeyPath},
		{"host hop", cfg.Jumphost.KeyPath},
		{"bastion hop", cfg.Jumphost.Jumphost.KeyPath},
	} {
		if hop.path != want {
			t.Errorf("%s KeyPath = %q, want %q", hop.name, hop.path, want)
		}
	}
}

// TestDPUSSHConfig_ThreeHopSeparateKey covers the ailab PoC shape: host
// is auth'd with one key (jumper.ed25519), bastion is auth'd with a
// different key (operator workstation). JumphostKeyRef overrides only
// the bastion hop's identity.
func TestDPUSSHConfig_ThreeHopSeparateKey(t *testing.T) {
	repo := dpuTestRepo(t)
	h := &poc.Host{
		SSH: poc.SSH{
			Address:        "198.18.0.21",
			Port:           22,
			User:           "ubuntu",
			KeyRef:         "keys/host.pem",
			Jumphost:       "10.196.23.100",
			JumphostUser:   "operator",
			JumphostKeyRef: "keys/workstation.pem",
		},
	}
	d := &poc.DPU{TmfifoIP: "192.168.100.2/30"}
	cfg, err := dpuSSHConfig(repo, h, d)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Jumphost.Jumphost.User != "operator" {
		t.Errorf("bastion user = %q, want operator (override)", cfg.Jumphost.Jumphost.User)
	}
	wantBastionKey := filepath.Join(repo, "keys/workstation.pem")
	if cfg.Jumphost.Jumphost.KeyPath != wantBastionKey {
		t.Errorf("bastion KeyPath = %q, want %q", cfg.Jumphost.Jumphost.KeyPath, wantBastionKey)
	}
	wantHostKey := filepath.Join(repo, "keys/host.pem")
	if cfg.Jumphost.KeyPath != wantHostKey {
		t.Errorf("host hop KeyPath = %q, want %q (unchanged from KeyRef)", cfg.Jumphost.KeyPath, wantHostKey)
	}
}
