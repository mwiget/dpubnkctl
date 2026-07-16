package provision

import (
	"context"
	"strings"
	"testing"

	"github.com/mwiget/dpubnkctl/internal/ssh"
)

// modeRunner returns a canned result for the first command whose text
// contains a registered substring; unmatched commands are empty success.
type modeRunner struct{ canned map[string]ssh.Result }

func (m modeRunner) Run(_ context.Context, cmd string) ssh.Result {
	for sub, r := range m.canned {
		if strings.Contains(cmd, sub) {
			return r
		}
	}
	return ssh.Result{ExitCode: 0}
}

func TestParseMode(t *testing.T) {
	cases := map[string]CPUMode{
		"nic": ModeNIC, "NIC": ModeNIC, " dpu ": ModeDPU, "DPU": ModeDPU,
	}
	for in, want := range cases {
		got, err := ParseMode(in)
		if err != nil || got != want {
			t.Fatalf("ParseMode(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := ParseMode("embedded"); err == nil {
		t.Fatal("ParseMode(\"embedded\") should error")
	}
}

// value/token/poc-string mapping must stay consistent — a swap here would
// flash the wrong personality onto the card.
func TestCPUModeMapping(t *testing.T) {
	if ModeDPU.value() != 1 || ModeNIC.value() != 0 {
		t.Fatalf("value(): DPU=%d NIC=%d, want 1/0", ModeDPU.value(), ModeNIC.value())
	}
	if ModeDPU.mlxToken() != "EMBEDDED_CPU" || ModeNIC.mlxToken() != "SEPARATED_HOST" {
		t.Fatalf("mlxToken(): DPU=%q NIC=%q", ModeDPU.mlxToken(), ModeNIC.mlxToken())
	}
	if ModeDPU.String() != "dpu" || ModeNIC.String() != "nic" {
		t.Fatalf("String(): DPU=%q NIC=%q", ModeDPU.String(), ModeNIC.String())
	}
}

// The set command must touch ONLY INTERNAL_CPU_MODEL — no ownership sub-knobs.
func TestSetCPUModeCmd(t *testing.T) {
	nic := SetCPUModeCmd("0000:03:00.0", ModeNIC)
	if !strings.Contains(nic, "mlxconfig -y -d 0000:03:00.0 set INTERNAL_CPU_MODEL=0") {
		t.Fatalf("NIC set cmd wrong: %q", nic)
	}
	dpu := SetCPUModeCmd("0000:03:00.0", ModeDPU)
	if !strings.Contains(dpu, "INTERNAL_CPU_MODEL=1") {
		t.Fatalf("DPU set cmd wrong: %q", dpu)
	}
	for _, subknob := range []string{"PAGE_SUPPLIER", "ESWITCH_MANAGER", "IB_VPORT0", "OFFLOAD_ENGINE"} {
		if strings.Contains(nic, subknob) || strings.Contains(dpu, subknob) {
			t.Fatalf("set cmd must not touch sub-knob %s", subknob)
		}
	}
}

// The apply step is a firmware reset (mlxfwreset), not a host reboot — a
// warm OS reboot does not reload INTERNAL_CPU_MODEL (verified live on BF3).
func TestFWResetCmd(t *testing.T) {
	got := FWResetCmd("0000:0d:00.0")
	if !strings.Contains(got, "mlxfwreset -d 0000:0d:00.0 -y reset") {
		t.Fatalf("FWResetCmd wrong: %q", got)
	}
	if strings.Contains(got, "reboot") {
		t.Fatalf("apply must be mlxfwreset, not reboot: %q", got)
	}
}

func TestReadCPUMode(t *testing.T) {
	// `mlxconfig -e q` lines are: [*] PARAM  Default  Current  NextBoot.
	// We must read the Current (+2) column, never Default.

	// DPU mode, values all equal → no leading "*".
	if m := ReadModeHelper(t, "        INTERNAL_CPU_MODEL       EMBEDDED_CPU(1)      EMBEDDED_CPU(1)      EMBEDDED_CPU(1)"); m != ModeDPU {
		t.Fatalf("all-EMBEDDED_CPU should read as ModeDPU, got %v", m)
	}

	// REGRESSION (real hardware, dpu-server-2 @ 0d:00.0): Default=EMBEDDED_CPU
	// but Current=SEPARATED_HOST. A naive substring match would wrongly say
	// DPU; the Current column says NIC.
	if m := ReadModeHelper(t, "*       INTERNAL_CPU_MODEL       EMBEDDED_CPU(1)      SEPARATED_HOST(0)    SEPARATED_HOST(0)"); m != ModeNIC {
		t.Fatalf("Default=EMBEDDED_CPU/Current=SEPARATED_HOST must read as ModeNIC (Current), got %v", m)
	}

	// Mirror case: Default=SEPARATED_HOST but Current=EMBEDDED_CPU → DPU.
	if m := ReadModeHelper(t, "*       INTERNAL_CPU_MODEL       SEPARATED_HOST(0)    EMBEDDED_CPU(1)      EMBEDDED_CPU(1)"); m != ModeDPU {
		t.Fatalf("Default=SEPARATED_HOST/Current=EMBEDDED_CPU must read as ModeDPU (Current), got %v", m)
	}

	// Command failure → error, ModeUnknown.
	r := modeRunner{canned: map[string]ssh.Result{"mlxconfig": {ExitCode: 1, Stderr: "mlxconfig: command not found"}}}
	if m, err := ReadCPUMode(context.Background(), r, "0000:03:00.0"); err == nil || m != ModeUnknown {
		t.Fatalf("failed read should return (ModeUnknown, err), got (%v, %v)", m, err)
	}
	// Unparseable output → error.
	r = modeRunner{canned: map[string]ssh.Result{"mlxconfig": {Stdout: "INTERNAL_CPU_MODEL   ???   ???   ???"}}}
	if _, err := ReadCPUMode(context.Background(), r, "0000:03:00.0"); err == nil {
		t.Fatal("unparseable current value should error")
	}
}

func ReadModeHelper(t *testing.T, stdout string) CPUMode {
	t.Helper()
	r := modeRunner{canned: map[string]ssh.Result{"mlxconfig": {Stdout: stdout}}}
	m, err := ReadCPUMode(context.Background(), r, "0000:03:00.0")
	if err != nil {
		t.Fatalf("ReadCPUMode error: %v", err)
	}
	return m
}
