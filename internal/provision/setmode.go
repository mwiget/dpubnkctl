package provision

import (
	"context"
	"fmt"
	"strings"

	"github.com/mwiget/dpubnkctl/internal/discover"
)

// CPUMode is the BlueField personality selected by the composite mlxconfig
// knob INTERNAL_CPU_MODEL. It is the ONLY knob that controls the mode — the
// ownership sub-knobs (INTERNAL_CPU_PAGE_SUPPLIER / ESWITCH_MANAGER /
// IB_VPORT0 / OFFLOAD_ENGINE) are deliberately left untouched; driving them
// individually is the old approach that needed a BMC cold cycle.
//
// Changing INTERNAL_CPU_MODEL only STAGES the new personality (Next Boot);
// applying it in either direction requires a firmware reset (mlxfwreset) —
// a warm host reboot is NOT sufficient (see FWResetCmd). rshim/tmfifo stay
// up in either mode, so the host can still reach the card for management.
type CPUMode int

const (
	ModeUnknown CPUMode = iota
	ModeDPU             // EMBEDDED_CPU(1): the Arm cores own the NIC; the DPU is a k8s node
	ModeNIC             // SEPARATED_HOST(0): the host owns the NIC; plain ConnectX behaviour
)

// String returns the poc.yaml `dpu.mode` token (dpu | nic); "" for unknown.
func (m CPUMode) String() string {
	switch m {
	case ModeDPU:
		return "dpu"
	case ModeNIC:
		return "nic"
	default:
		return "unknown"
	}
}

// mlxToken is the mlxconfig display token INTERNAL_CPU_MODEL reports for
// this mode (EMBEDDED_CPU / SEPARATED_HOST).
func (m CPUMode) mlxToken() string {
	switch m {
	case ModeDPU:
		return "EMBEDDED_CPU"
	case ModeNIC:
		return "SEPARATED_HOST"
	default:
		return ""
	}
}

// value is the integer INTERNAL_CPU_MODEL mlxconfig writes (1=DPU, 0=NIC).
func (m CPUMode) value() int {
	if m == ModeDPU {
		return 1
	}
	return 0
}

// Describe is an operator-facing one-liner, e.g. "EMBEDDED_CPU(1) / DPU mode".
func (m CPUMode) Describe() string {
	switch m {
	case ModeDPU:
		return "EMBEDDED_CPU(1) / DPU mode"
	case ModeNIC:
		return "SEPARATED_HOST(0) / NIC mode"
	default:
		return "unknown"
	}
}

// ParseMode maps the CLI --mode token (case-insensitive nic | dpu) to a CPUMode.
func ParseMode(s string) (CPUMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "dpu":
		return ModeDPU, nil
	case "nic":
		return ModeNIC, nil
	default:
		return ModeUnknown, fmt.Errorf("invalid mode %q (want nic | dpu)", s)
	}
}

// SetCPUModeCmd returns the host-side mlxconfig write that STAGES the mode
// change (into the Next Boot column). The live value only changes after a
// firmware reset — see FWResetCmd. Only INTERNAL_CPU_MODEL is set (never the
// ownership sub-knobs). Runs against the DPU's host-side PCI address, same
// as the readiness probe.
func SetCPUModeCmd(dpuPCI string, m CPUMode) string {
	return fmt.Sprintf("sudo -n mlxconfig -y -d %s set INTERNAL_CPU_MODEL=%d", dpuPCI, m.value())
}

// FWResetCmd returns the host-side mlxfwreset that APPLIES a staged
// INTERNAL_CPU_MODEL change to the live (Current) value.
//
// A plain OS `reboot` is NOT sufficient: the BlueField keeps its running
// firmware config across a warm host reboot, so Current never changes
// (verified live on BF3 — a host reboot left Current=SEPARATED_HOST while
// Next Boot=EMBEDDED_CPU). mlxfwreset performs an actual firmware reset
// (default level 3: driver restart + PCI reset) that reloads NVRAM and
// flips Current. The host's own SSH survives when management isn't carried
// by the DPU being reset; where it is, a cold power cycle via BMC is the
// alternative the operator must run.
func FWResetCmd(dpuPCI string) string {
	return fmt.Sprintf("sudo -n mlxfwreset -d %s -y reset", dpuPCI)
}

// ReadCPUMode reads INTERNAL_CPU_MODEL for the DPU at dpuPCI via the host
// runner and reports the LIVE current mode. Returns an error if the knob
// can't be read (mlxconfig missing, wrong PCI) or the output can't be
// parsed, so callers can tell "unreadable" apart from a definite mode.
//
// It uses `mlxconfig -e q`, which prints three value columns
// (Default | Current | Next Boot); a leading "*" marks Current != Default:
//
//	INTERNAL_CPU_MODEL   EMBEDDED_CPU(1)   SEPARATED_HOST(0)   SEPARATED_HOST(0)
//
// The Current column (index +2 after the param name) is the running value;
// Next Boot is the staged value that an mlxfwreset will apply. We MUST read
// Current — a plain substring match would hit the Default column, and the
// Next Boot column would report a staged change as done before the mlxfwreset
// that applies it. Verification after `set` therefore only passes once the
// mlxfwreset has made Current match the target.
func ReadCPUMode(ctx context.Context, runner discover.Runner, dpuPCI string) (CPUMode, error) {
	cmd := fmt.Sprintf("sudo -n mlxconfig -d %s -e q 2>/dev/null | grep INTERNAL_CPU_MODEL || true", dpuPCI)
	r := runner.Run(ctx, cmd)
	if !r.OK() {
		return ModeUnknown, fmt.Errorf("read INTERNAL_CPU_MODEL for %s: %s", dpuPCI, strings.TrimSpace(r.Stderr+r.Stdout))
	}
	return parseCurrentMode(r.Stdout, dpuPCI)
}

// parseCurrentMode extracts the Current column from a `mlxconfig -e q`
// INTERNAL_CPU_MODEL line. Fields are whitespace-split; the value columns
// are Default (+1), Current (+2), Next Boot (+3) after the "INTERNAL_CPU_MODEL"
// token — matching by name makes it robust to the optional leading "*"
// (present only when Current != Default).
func parseCurrentMode(out, dpuPCI string) (CPUMode, error) {
	fields := strings.Fields(out)
	for i, f := range fields {
		if f != "INTERNAL_CPU_MODEL" {
			continue
		}
		if i+2 >= len(fields) {
			break
		}
		cur := fields[i+2] // Current column (live value)
		switch {
		case strings.Contains(cur, ModeDPU.mlxToken()):
			return ModeDPU, nil
		case strings.Contains(cur, ModeNIC.mlxToken()):
			return ModeNIC, nil
		}
		return ModeUnknown, fmt.Errorf("unrecognized INTERNAL_CPU_MODEL current value %q for %s", cur, dpuPCI)
	}
	return ModeUnknown, fmt.Errorf("could not parse INTERNAL_CPU_MODEL for %s (got %q)", dpuPCI, strings.TrimSpace(out))
}
