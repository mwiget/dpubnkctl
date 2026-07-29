package cli

import (
	"strings"
	"testing"

	"github.com/mwiget/dpubnkctl/internal/poc"
)

func TestSuggestRole(t *testing.T) {
	cases := []struct {
		name           string
		dpuCount       int
		totalReachable int
		noDPUHosts     int
		wantRole       string
		wantInRation   string
	}{
		{
			name:           "no-DPU host suggested CP",
			dpuCount:       0,
			totalReachable: 3,
			noDPUHosts:     1,
			wantRole:       "control-plane",
			wantInRation:   "no DPU",
		},
		{
			name:           "DPU host with 3 CP-free siblings → worker",
			dpuCount:       1,
			totalReachable: 5,
			noDPUHosts:     3,
			wantRole:       "worker",
			wantInRation:   "DPU-free hosts available as dedicated control planes",
		},
		{
			name:           "DPU host with 4 CP-free siblings → worker",
			dpuCount:       2,
			totalReachable: 6,
			noDPUHosts:     4,
			wantRole:       "worker",
			wantInRation:   "this host is worker only",
		},
		{
			name:           "single-host lab (1 DPU host, 0 CP-free) → both",
			dpuCount:       1,
			totalReachable: 1,
			noDPUHosts:     0,
			wantRole:       "both",
			wantInRation:   "single-host lab",
		},
		{
			name:           "small lab (2 DPU hosts, 0 CP-free) → both",
			dpuCount:       1,
			totalReachable: 2,
			noDPUHosts:     0,
			wantRole:       "both",
			wantInRation:   "runs control plane and worker",
		},
		{
			name:           "4 hosts mixed (2 DPU + 2 CP-free) → both for DPU host",
			dpuCount:       1,
			totalReachable: 4,
			noDPUHosts:     2,
			wantRole:       "both",
			wantInRation:   "fewer than the 3 control-plane-only nodes etcd needs for HA",
		},
		{
			name:           "4 hosts mixed (2 DPU + 2 CP-free) → control-plane for DPU-free host",
			dpuCount:       0,
			totalReachable: 4,
			noDPUHosts:     2,
			wantRole:       "control-plane",
			wantInRation:   "no DPU",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			role, rationale := suggestRole(c.dpuCount, c.totalReachable, c.noDPUHosts)
			if role != c.wantRole {
				t.Errorf("role = %q, want %q", role, c.wantRole)
			}
			if !strings.Contains(rationale, c.wantInRation) {
				t.Errorf("rationale = %q, want substring %q", rationale, c.wantInRation)
			}
		})
	}
}

// TestFillDPUWizardDefaults_MultiDPUHostnamesUnique — issue #18, Tokyo
// lab: a host with two BF3s had both DPUs defaulted to <host>-bf3, so
// the second kubeadm join would take over the first's Node object.
func TestFillDPUWizardDefaults_MultiDPUHostnamesUnique(t *testing.T) {
	p := &poc.PoC{Hosts: []poc.Host{{
		Name: "dpu-server-2",
		SSH:  poc.SSH{Address: "10.0.0.5"},
		DPUs: []poc.DPU{{PCI: "0000:03:00.0"}, {PCI: "0000:83:00.0"}},
	}}}
	fillDPUWizardDefaults(p, "10.0.0.5", "dpu-server-2")

	got := []string{p.Hosts[0].DPUs[0].Hostname, p.Hosts[0].DPUs[1].Hostname}
	if got[0] == got[1] {
		t.Fatalf("both DPUs got the same hostname %q", got[0])
	}
	for i, want := range []string{"dpu-server-2-bf3-1", "dpu-server-2-bf3-2"} {
		if got[i] != want {
			t.Errorf("DPU %d hostname = %q, want %q", i, got[i], want)
		}
	}
}

// TestFillDPUWizardDefaults_SingleDPUKeepsHistoricalName — the one-DPU
// host must keep <host>-bf3. That name is in every existing PoC and
// example; suffixing it would rename nodes under running clusters on
// the next wizard run.
func TestFillDPUWizardDefaults_SingleDPUKeepsHistoricalName(t *testing.T) {
	p := &poc.PoC{Hosts: []poc.Host{{
		Name: "worker1",
		SSH:  poc.SSH{Address: "10.0.0.6"},
		DPUs: []poc.DPU{{PCI: "0000:03:00.0"}},
	}}}
	fillDPUWizardDefaults(p, "10.0.0.6", "worker1")
	if got := p.Hosts[0].DPUs[0].Hostname; got != "worker1-bf3" {
		t.Errorf("single-DPU hostname = %q, want %q", got, "worker1-bf3")
	}
}

// TestFillDPUWizardDefaults_DoesNotClobberOrCollide — an operator-set
// name is preserved, and the generated name for its neighbour must not
// collide with it.
func TestFillDPUWizardDefaults_DoesNotClobberOrCollide(t *testing.T) {
	p := &poc.PoC{Hosts: []poc.Host{{
		Name: "host9",
		SSH:  poc.SSH{Address: "10.0.0.7"},
		DPUs: []poc.DPU{
			{PCI: "0000:03:00.0", Hostname: "host9-bf3-1"}, // operator's choice
			{PCI: "0000:83:00.0"},                          // to be filled
		},
	}}}
	fillDPUWizardDefaults(p, "10.0.0.7", "host9")

	if got := p.Hosts[0].DPUs[0].Hostname; got != "host9-bf3-1" {
		t.Errorf("operator-set hostname was clobbered: %q", got)
	}
	if got := p.Hosts[0].DPUs[1].Hostname; got == "host9-bf3-1" {
		t.Errorf("generated hostname collided with the operator's: %q", got)
	}
}
