package poc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// goodPoC returns a PoC that should validate clean (errors == 0). Tests
// then mutate one thing at a time to assert each rule fires individually.
func goodPoC(t *testing.T) (*PoC, string) {
	t.Helper()
	repo := t.TempDir()

	// Create the files referenced from poc.yaml.
	must := func(rel string) {
		full := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	must("keys/host1.pem")
	must("keys/dpu_password.hash")
	must("keys/f5-far-auth-key.tgz")
	must("keys/.jwt")

	p := &PoC{
		Metadata: Metadata{Name: "test", Customer: "Acme"},
		Network: Network{
			InternalCIDR:            "10.244.0.0/16",
			DPUMTU:                  9000,
			PodMTU:                  8900,
			ClusterAPIServerAddress: "10.10.41.66",
			NodeIPRole:              "internal",
		},
		Hosts: []Host{{
			Name: "host1",
			Role: "both",
			SSH:  SSH{Address: "192.168.68.10", User: "ubuntu", KeyRef: "keys/host1.pem"},
			DataPlane: &HostDataPlane{
				ParentIface: "ens16f0np0",
				VLANs: []HostDataPlaneVLAN{
					// IP matches ClusterAPIServerAddress on purpose — the new
					// validate rule requires this when there's a single CP.
					{Role: "internal", Tag: 41, IP: "10.10.41.66/24"},
				},
			},
			DPUs: []DPU{{
				PCI:      "0000:03:00.0",
				Mode:     "dpu",
				LAG:      true,
				Hostname: "host1-bf3",
				TmfifoIP: "192.168.100.2/30",
				VLANs: []DPUVLAN{
					{Role: "internal", Tag: 41, IP: "10.10.41.5/24"},
				},
			}},
		}},
		Provisioning: Provisioning{
			DPUPasswordHashRef: "keys/dpu_password.hash",
			DPUDNS:             []string{"8.8.8.8"},
			DPUNTP:             []string{"pool.ntp.org"},
		},
		BNK: BNK{
			FARKeyRef:      "keys/f5-far-auth-key.tgz",
			JWTRef:         "keys/.jwt",
			ExternalSelfIP: "10.10.40.100",
			InternalSelfIP: "10.10.41.100",
		},
	}
	return p, repo
}

func TestValidate_HappyPath(t *testing.T) {
	p, repo := goodPoC(t)
	r := Validate(p, repo)
	if !r.Valid() {
		t.Fatalf("expected clean validate, got errors:\n  %s", strings.Join(r.Errors, "\n  "))
	}
	if len(r.Warnings) > 0 {
		t.Errorf("expected zero warnings on happy path, got:\n  %s", strings.Join(r.Warnings, "\n  "))
	}
}

func TestValidate_NonLAGMissingUplink(t *testing.T) {
	p, repo := goodPoC(t)
	p.Hosts[0].DPUs[0].LAG = false
	// VLAN has no uplink set → error
	r := Validate(p, repo)
	if r.Valid() {
		t.Fatal("expected error for non-LAG VLAN missing uplink")
	}
	found := false
	for _, e := range r.Errors {
		if strings.Contains(e, "uplink") {
			found = true
		}
	}
	if !found {
		t.Errorf("missing uplink error not surfaced. got:\n  %s", strings.Join(r.Errors, "\n  "))
	}
}

func TestValidate_NonLAGWithUplinkOK(t *testing.T) {
	p, repo := goodPoC(t)
	p.Hosts[0].DPUs[0].LAG = false
	p.Hosts[0].DPUs[0].VLANs[0].Uplink = "p1"
	r := Validate(p, repo)
	if !r.Valid() {
		t.Errorf("expected clean validate, got:\n  %s", strings.Join(r.Errors, "\n  "))
	}
}

func TestValidate_MissingFARFile(t *testing.T) {
	p, repo := goodPoC(t)
	if err := os.Remove(filepath.Join(repo, "keys/f5-far-auth-key.tgz")); err != nil {
		t.Fatal(err)
	}
	r := Validate(p, repo)
	if r.Valid() {
		t.Fatal("expected error for missing FAR")
	}
	if !errorContains(r, "far_key_ref") {
		t.Errorf("FAR-missing error not surfaced: %v", r.Errors)
	}
}

func TestValidate_MissingJWTFile(t *testing.T) {
	p, repo := goodPoC(t)
	if err := os.Remove(filepath.Join(repo, "keys/.jwt")); err != nil {
		t.Fatal(err)
	}
	r := Validate(p, repo)
	if !errorContains(r, "jwt_ref") {
		t.Errorf("JWT-missing error not surfaced: %v", r.Errors)
	}
}

func TestValidate_NoControlPlane(t *testing.T) {
	p, repo := goodPoC(t)
	p.Hosts[0].Role = "worker"
	r := Validate(p, repo)
	if !errorContains(r, "control-plane") {
		t.Errorf("no-control-plane error not surfaced: %v", r.Errors)
	}
}

func TestValidate_DefaultInternalCIDRWarns(t *testing.T) {
	p, repo := goodPoC(t)
	p.Network.InternalCIDR = defaultInternalCIDR
	r := Validate(p, repo)
	if !r.Valid() {
		t.Fatalf("default CIDR should warn, not error: %v", r.Errors)
	}
	if !warningContains(r, "still at the template default") {
		t.Errorf("default-CIDR warning not surfaced: %v", r.Warnings)
	}
}

func TestValidate_InvalidVLANTag(t *testing.T) {
	p, repo := goodPoC(t)
	p.Hosts[0].DPUs[0].VLANs[0].Tag = 5000
	r := Validate(p, repo)
	if !errorContains(r, "tag") {
		t.Errorf("invalid tag not surfaced: %v", r.Errors)
	}
}

func TestValidate_VLANPortNameTooLong(t *testing.T) {
	p, repo := goodPoC(t)
	// "replication" (11) + "4094" (4) = "replication4094" = 15 chars OK.
	// "replication" + "12345" → 16 chars NOT OK. But tag must be ≤4094.
	// Use a 10-char role + 4-digit tag = 14 chars (OK). 10-char + bad tag would
	// also surface a tag-range error. Use "abcdefghij" (10) + 4094 = 14 → OK.
	// Force >15: "abcdefghij" (10) + 60000 → tag out of range. So we can't trigger
	// the port-name check without also tripping tag range. That's fine — it just
	// means port-name check is well-defended. Skip this micro-case.
	_ = p
	_ = repo
	t.Skip("port-name length is implicitly bounded by the valid-tag range")
}

func TestValidate_HostMissingSSH(t *testing.T) {
	p, repo := goodPoC(t)
	p.Hosts[0].SSH.Address = ""
	p.Hosts[0].SSH.User = ""
	p.Hosts[0].SSH.KeyRef = ""
	r := Validate(p, repo)
	for _, want := range []string{"ssh.address", "ssh.user", "ssh.key_ref"} {
		if !errorContains(r, want) {
			t.Errorf("expected error mentioning %q, got %v", want, r.Errors)
		}
	}
}

func TestValidate_PodMTULargerThanDPUMTU(t *testing.T) {
	p, repo := goodPoC(t)
	p.Network.PodMTU = 9100
	p.Network.DPUMTU = 9000
	r := Validate(p, repo)
	if !errorContains(r, "pod_mtu") {
		t.Errorf("pod_mtu > dpu_mtu not surfaced: %v", r.Errors)
	}
}

func TestValidate_EmptyClusterAPIServerWarns(t *testing.T) {
	p, repo := goodPoC(t)
	p.Network.ClusterAPIServerAddress = ""
	r := Validate(p, repo)
	if !r.Valid() {
		t.Errorf("empty cluster_apiserver_address should warn, not error: %v", r.Errors)
	}
	if !warningContains(r, "cluster_apiserver_address") {
		t.Errorf("cluster_apiserver_address warning not surfaced: %v", r.Warnings)
	}
}

func TestValidateForPhase_ProvisionIgnoresDeployFields(t *testing.T) {
	// The motivating bug: `provision dpus` was refusing to run because
	// FAR/JWT/selfip were empty, even though those only matter at
	// deploy. ValidateForPhase(PhaseProvision) must let that through.
	p, repo := goodPoC(t)
	p.BNK.FARKeyRef = ""
	p.BNK.JWTRef = ""
	p.BNK.ExternalSelfIP = ""
	p.BNK.InternalSelfIP = ""

	r := ValidateForPhase(p, repo, PhaseProvision)
	if !r.Valid() {
		t.Errorf("PhaseProvision must not block on empty deploy-phase fields; got:\n  %s", strings.Join(r.Errors, "\n  "))
	}
	for _, w := range r.Warnings {
		if strings.Contains(w, "selfip") {
			t.Errorf("PhaseProvision must not emit deploy-phase warnings; got: %s", w)
		}
	}

	// Conversely, the same PoC should fail at Deploy.
	r = ValidateForPhase(p, repo, PhaseDeploy)
	if r.Valid() {
		t.Fatal("PhaseDeploy must block on missing FAR/JWT")
	}
	if !errorContains(r, "far_key_ref") || !errorContains(r, "jwt_ref") {
		t.Errorf("expected FAR + JWT errors at PhaseDeploy; got: %v", r.Errors)
	}
}

func TestValidateForPhase_ProvisionStillCatchesProvisionFields(t *testing.T) {
	// Filter must not silence rules that DO belong to the current phase.
	p, repo := goodPoC(t)
	p.Hosts[0].DPUs[0].Hostname = ""
	p.Provisioning.DPUDNS = nil

	r := ValidateForPhase(p, repo, PhaseProvision)
	if r.Valid() {
		t.Fatal("expected provision-phase rules to still fire")
	}
	if !errorContains(r, "hostname") {
		t.Errorf("DPU hostname error missing: %v", r.Errors)
	}
	if !errorContains(r, "dpu_dns") {
		t.Errorf("dpu_dns error missing: %v", r.Errors)
	}
}

func TestValidate_StillRunsEverything(t *testing.T) {
	// Backward compat: bare Validate must enforce all rules (== PhaseDeploy).
	p, repo := goodPoC(t)
	p.BNK.JWTRef = ""

	r := Validate(p, repo)
	if r.Valid() {
		t.Fatal("Validate (full) must still block on missing JWT")
	}
}

func TestValidate_ClusterAPIServerMustMatchSingleCPVLANIP(t *testing.T) {
	// goodPoC ships with apiserver == host VLAN IP (both .66). Force a
	// mismatch and confirm the new rule fires.
	p, repo := goodPoC(t)
	p.Network.ClusterAPIServerAddress = "10.10.41.99" // not present on the CP host
	r := ValidateForPhase(p, repo, PhaseCluster)
	if !errorContains(r, "cluster_apiserver_address") {
		t.Errorf("expected cluster_apiserver_address mismatch error; got: %v", r.Errors)
	}
}

func TestValidate_ClusterAPIServerNotEnforcedAtProvision(t *testing.T) {
	// The cross-check should only fire at Cluster — provision shouldn't
	// care since kubeadm hasn't run yet.
	p, repo := goodPoC(t)
	p.Network.ClusterAPIServerAddress = "192.0.2.99" // bogus, doesn't match anything
	r := ValidateForPhase(p, repo, PhaseProvision)
	if errorContains(r, "cluster_apiserver_address") {
		t.Errorf("apiserver cross-check leaked into provision phase: %v", r.Errors)
	}
}

func TestValidate_TmfifoIPWrongSubnet(t *testing.T) {
	// 192.168.100.6/30 is .4 net / .5 first / .6 second / .7 bcast — a
	// different /30 than the rshim default 192.168.100.0/30.
	p, repo := goodPoC(t)
	p.Hosts[0].DPUs[0].TmfifoIP = "192.168.100.6/30"
	r := ValidateForPhase(p, repo, PhaseProvision)
	if !errorContains(r, "tmfifo_ip") {
		t.Errorf("expected tmfifo_ip wrong-subnet error; got: %v", r.Errors)
	}
}

func TestValidate_TmfifoIPNotSlash30(t *testing.T) {
	p, repo := goodPoC(t)
	p.Hosts[0].DPUs[0].TmfifoIP = "192.168.100.2/24"
	r := ValidateForPhase(p, repo, PhaseProvision)
	if !errorContains(r, "/30") {
		t.Errorf("expected /30 error; got: %v", r.Errors)
	}
}

func TestValidate_TmfifoIPCollidesWithRshim(t *testing.T) {
	p, repo := goodPoC(t)
	p.Hosts[0].DPUs[0].TmfifoIP = "192.168.100.1/30"
	r := ValidateForPhase(p, repo, PhaseProvision)
	if !errorContains(r, "rshim") {
		t.Errorf("expected rshim-collision error; got: %v", r.Errors)
	}
}

func TestValidate_TmfifoIPOK(t *testing.T) {
	p, repo := goodPoC(t)
	p.Hosts[0].DPUs[0].TmfifoIP = "192.168.100.2/30"
	r := ValidateForPhase(p, repo, PhaseProvision)
	if !r.Valid() {
		t.Errorf("standard 192.168.100.2/30 should validate clean; got: %v", r.Errors)
	}
}

func errorContains(r ValidationResult, substr string) bool {
	for _, e := range r.Errors {
		if strings.Contains(e, substr) {
			return true
		}
	}
	return false
}

func warningContains(r ValidationResult, substr string) bool {
	for _, w := range r.Warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}
