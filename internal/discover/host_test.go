package discover

import (
	"context"
	"strings"
	"testing"

	"github.com/mwiget/dpubnkctl/internal/ssh"
)

// fakeRunner returns canned output for prefix-matched commands. It records
// every command it sees so tests can assert on the probe sequence.
type fakeRunner struct {
	canned map[string]ssh.Result
	seen   []string
}

func (f *fakeRunner) Run(_ context.Context, cmd string) ssh.Result {
	f.seen = append(f.seen, cmd)
	for prefix, r := range f.canned {
		if strings.HasPrefix(cmd, prefix) {
			return r
		}
	}
	// Default: empty success — many probes treat that as "tool absent".
	return ssh.Result{ExitCode: 0}
}

func ok(out string) ssh.Result { return ssh.Result{Stdout: out} }

func TestProbeHostInfo_BareHost(t *testing.T) {
	f := &fakeRunner{canned: map[string]ssh.Result{
		"uname -r":             ok("5.15.0-generic"),
		"hostname":             ok("test-host\n"),
		"cat /etc/os-release":  ok(`ID=ubuntu` + "\n" + `PRETTY_NAME="Ubuntu 22.04 LTS"` + "\n"),
		"ip -j addr":           ok(`[{"ifname":"eno1","mtu":1500,"address":"00:11:22:33:44:55","addr_info":[{"local":"10.0.0.5"}]}]`),
	}}
	h, _ := probeHostInfo(context.Background(), f)
	if h.Hostname != "test-host" {
		t.Errorf("Hostname=%q", h.Hostname)
	}
	if h.OS.ID != "ubuntu" {
		t.Errorf("OS.ID=%q", h.OS.ID)
	}
	if len(h.Interfaces) != 1 || h.Interfaces[0].Name != "eno1" {
		t.Errorf("interfaces wrong: %+v", h.Interfaces)
	}
	if h.Tools.Mlxconfig != "" {
		t.Errorf("expected no mlxconfig, got %q", h.Tools.Mlxconfig)
	}
}

func TestDiscoverHost_NoDPU(t *testing.T) {
	f := &fakeRunner{canned: map[string]ssh.Result{
		"uname -r":            ok("6.8.0-generic"),
		"hostname":            ok("plain-host"),
		"cat /etc/os-release": ok(`ID=ubuntu` + "\n"),
		"ip -j addr":          ok(`[]`),
		// No mlxconfig, no ipmitool, no DPUs.
		"lspci -nn -d 15b3:": ok(""),
	}}
	r, err := DiscoverHost(context.Background(), HostOptions{Address: "10.0.0.5", Runner: f})
	if err != nil {
		t.Fatalf("DiscoverHost: %v", err)
	}
	if r.Host.Hostname != "plain-host" {
		t.Errorf("hostname = %q", r.Host.Hostname)
	}
	if len(r.DPUs) != 0 {
		t.Errorf("expected no DPUs, got %d", len(r.DPUs))
	}
	if got := r.Classification(); got != "host without DPU" {
		t.Errorf("Classification = %q", got)
	}
}

func TestProbeDPUs_WithMlxconfig(t *testing.T) {
	mlxOut := `Configurations:                                    Default                Current                Next Boot
        INTERNAL_CPU_MODEL                          EMBEDDED_CPU(1)        EMBEDDED_CPU(1)        EMBEDDED_CPU(1)
        LAG_RESOURCE_ALLOCATION                     DISABLE(0)             ENABLE(1)              ENABLE(1)
`
	f := &fakeRunner{canned: map[string]ssh.Result{
		"lspci -nn -d 15b3:": ok("03:00.0 Ethernet controller [0200]: Mellanox Technologies BlueField-3 [15b3:a2dc] (rev 01)\n03:00.1 Ethernet controller [0200]: Mellanox Technologies BlueField-3 [15b3:a2dc] (rev 01)\n"),
		"mlxconfig -d 03:00.0": ok(mlxOut),
		"cat /dev/rshim0/misc": {ExitCode: 0}, // empty
	}}
	tools := Tools{Mlxconfig: "/usr/bin/mlxconfig"}
	dpus, isDPU, _ := probeDPUs(context.Background(), f, tools)
	if isDPU {
		t.Errorf("expected server-host classification, got isDPU=true")
	}
	if len(dpus) != 1 {
		t.Fatalf("expected 1 merged card, got %d", len(dpus))
	}
	if dpus[0].PCI != "03:00.0" {
		t.Errorf("PCI=%q", dpus[0].PCI)
	}
	if dpus[0].Mlxconfig == nil {
		t.Fatal("Mlxconfig parsed as nil")
	}
	if dpus[0].Mlxconfig.LAGResourceAllocation != "ENABLE" {
		t.Errorf("LAG=%q", dpus[0].Mlxconfig.LAGResourceAllocation)
	}
}

func TestProbeDPUs_WithoutMlxconfig(t *testing.T) {
	f := &fakeRunner{canned: map[string]ssh.Result{
		"lspci -nn -d 15b3:": ok("03:00.0 Ethernet controller [0200]: Mellanox Technologies BlueField-3 [15b3:a2dc] (rev 01)\n"),
	}}
	tools := Tools{} // no mlxconfig
	dpus, isDPU, warnings := probeDPUs(context.Background(), f, tools)
	if isDPU {
		t.Errorf("expected server-host classification, got isDPU=true")
	}
	if len(dpus) != 1 {
		t.Fatalf("got %d, want 1", len(dpus))
	}
	if dpus[0].Mlxconfig != nil {
		t.Errorf("expected nil Mlxconfig with no MFT, got %+v", dpus[0].Mlxconfig)
	}
	foundWarn := false
	for _, w := range warnings {
		if strings.Contains(w, "mlxconfig not installed") {
			foundWarn = true
		}
	}
	if !foundWarn {
		t.Errorf("expected mlxconfig-missing warning, got %v", warnings)
	}
}

