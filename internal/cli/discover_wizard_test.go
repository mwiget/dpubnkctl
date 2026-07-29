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

// TestDefaultDPUHostname_StaysWithinRFC1123 — Host.Name may legally be
// up to 63 chars, and appending "-bf3-2" used to push the generated DPU
// hostname past the limit poc.validate enforces. The wizard would then
// write a poc.yaml its own validator rejects, with the error pointing at
// a field the operator never set.
func TestDefaultDPUHostname_StaysWithinRFC1123(t *testing.T) {
	for _, hostLen := range []int{10, 55, 58, 63} {
		host := strings.Repeat("a", hostLen)
		for _, total := range []int{1, 2} {
			for idx := 0; idx < total; idx++ {
				got := defaultDPUHostname(host, idx, total, map[string]bool{})
				if len(got) > 63 {
					t.Errorf("hostLen=%d total=%d idx=%d: generated %d-char name %q exceeds the 63-char RFC 1123 limit",
						hostLen, total, idx, len(got), got)
				}
				if strings.HasSuffix(got, "-") {
					t.Errorf("hostLen=%d: generated %q ends in '-', not a valid RFC 1123 label", hostLen, got)
				}
			}
		}
	}
}

// Long host names must still yield DISTINCT names per DPU after
// truncation — clamping mustn't collapse two DPUs onto one name, which
// is the very collision this whole change exists to prevent.
func TestDefaultDPUHostname_LongNamesStayUnique(t *testing.T) {
	host := strings.Repeat("a", 63)
	taken := map[string]bool{}
	seen := map[string]bool{}
	for idx := 0; idx < 3; idx++ {
		got := defaultDPUHostname(host, idx, 3, taken)
		if seen[got] {
			t.Fatalf("idx=%d produced duplicate name %q", idx, got)
		}
		seen[got] = true
		taken[got] = true
	}
	t.Logf("distinct truncated names: %v", len(seen))
}

// TestFillDPUWizardDefaults_MultiDPUTmfifoDistinct — issue #20. Every DPU
// on a host used to get 192.168.100.2/30 on tmfifo_net0, so dpubnkctl
// addressed one card repeatedly and silently skipped the rest.
func TestFillDPUWizardDefaults_MultiDPUTmfifoDistinct(t *testing.T) {
	p := &poc.PoC{Hosts: []poc.Host{{
		Name: "dpu-server-2",
		SSH:  poc.SSH{Address: "10.0.0.5"},
		DPUs: []poc.DPU{{PCI: "0000:03:00.0"}, {PCI: "0000:83:00.0"}},
	}}}
	fillDPUWizardDefaults(p, "10.0.0.5", "dpu-server-2")
	d := p.Hosts[0].DPUs

	if d[0].TmfifoIP == d[1].TmfifoIP {
		t.Fatalf("both DPUs got tmfifo_ip %q", d[0].TmfifoIP)
	}
	if d[0].TmfifoNetIface() == d[1].TmfifoNetIface() {
		t.Fatalf("both DPUs got iface %q", d[0].TmfifoNetIface())
	}
	// First DPU keeps the rshim driver's own defaults so single-DPU PoCs
	// and existing poc.yaml files are unaffected.
	if d[0].TmfifoIP != "192.168.100.2/30" {
		t.Errorf("DPU0 tmfifo_ip = %q, want 192.168.100.2/30", d[0].TmfifoIP)
	}
	if d[0].TmfifoIface != "" {
		t.Errorf("DPU0 tmfifo_iface should stay implicit (tmfifo_net0), got %q", d[0].TmfifoIface)
	}
	if d[1].TmfifoIP != "192.168.100.6/30" {
		t.Errorf("DPU1 tmfifo_ip = %q, want 192.168.100.6/30 (next /30)", d[1].TmfifoIP)
	}
	if d[1].TmfifoIface != "tmfifo_net1" {
		t.Errorf("DPU1 tmfifo_iface = %q, want tmfifo_net1", d[1].TmfifoIface)
	}
	// The host end of each link must land in the matching /30.
	if got := d[1].TmfifoHostIP(); got != "192.168.100.5/30" {
		t.Errorf("DPU1 host-side = %q, want 192.168.100.5/30", got)
	}
}

// Single-DPU hosts must be byte-for-byte unchanged — this is every
// existing PoC in the wild.
func TestFillDPUWizardDefaults_SingleDPUTmfifoUnchanged(t *testing.T) {
	p := &poc.PoC{Hosts: []poc.Host{{
		Name: "worker1",
		SSH:  poc.SSH{Address: "10.0.0.6"},
		DPUs: []poc.DPU{{PCI: "0000:03:00.0"}},
	}}}
	fillDPUWizardDefaults(p, "10.0.0.6", "worker1")
	if got := p.Hosts[0].DPUs[0].TmfifoIP; got != "192.168.100.2/30" {
		t.Errorf("tmfifo_ip = %q, want 192.168.100.2/30", got)
	}
	if got := p.Hosts[0].DPUs[0].TmfifoIface; got != "" {
		t.Errorf("tmfifo_iface should stay implicit, got %q", got)
	}
}

// Operator-pinned tmfifo values must survive, and generated ones must
// dodge them rather than colliding.
func TestFillDPUWizardDefaults_TmfifoDoesNotClobberOrCollide(t *testing.T) {
	p := &poc.PoC{Hosts: []poc.Host{{
		Name: "host9",
		SSH:  poc.SSH{Address: "10.0.0.7"},
		DPUs: []poc.DPU{
			{PCI: "0000:03:00.0", TmfifoIP: "192.168.100.6/30", TmfifoIface: "tmfifo_net1"},
			{PCI: "0000:83:00.0"},
		},
	}}}
	fillDPUWizardDefaults(p, "10.0.0.7", "host9")
	d := p.Hosts[0].DPUs

	if d[0].TmfifoIP != "192.168.100.6/30" || d[0].TmfifoIface != "tmfifo_net1" {
		t.Errorf("operator-set tmfifo was clobbered: ip=%q iface=%q", d[0].TmfifoIP, d[0].TmfifoIface)
	}
	if d[1].TmfifoIP == d[0].TmfifoIP {
		t.Errorf("generated tmfifo_ip collided with the operator's: %q", d[1].TmfifoIP)
	}
	if d[1].TmfifoNetIface() == d[0].TmfifoNetIface() {
		t.Errorf("generated iface collided with the operator's: %q", d[1].TmfifoNetIface())
	}
}
