package provision

import (
	"context"
	"fmt"
	"strings"

	"github.com/mwiget/dpubnkctl/internal/discover"
	"github.com/mwiget/dpubnkctl/internal/ssh"
)

// ReadinessReport summarizes whether a host is ready to flash one DPU.
// Status is "ready" if Errors is empty; warnings don't block.
type ReadinessReport struct {
	Host     string
	DPU      string
	Errors   []string
	Warnings []string
	Checks   []ReadinessCheck
}

func (r ReadinessReport) Ready() bool { return len(r.Errors) == 0 }

type ReadinessCheck struct {
	Name   string
	Result string // ok | warn | fail
	Detail string
}

// Check runs the pre-flash readiness probes against `host` and reports
// whether `dpu` is in a state where bfb-install can proceed.
func Check(ctx context.Context, host string, runner discover.Runner, dpuPCI string) ReadinessReport {
	rep := ReadinessReport{Host: host, DPU: dpuPCI}

	// 1. Sudo / root.
	if r := runner.Run(ctx, "sudo -n true 2>&1"); !r.OK() {
		rep.Errors = append(rep.Errors, "passwordless sudo not available — required to run bfb-install, mlxconfig, and write /dev/rshim0/boot")
		rep.Checks = append(rep.Checks, ReadinessCheck{"sudo -n", "fail", strings.TrimSpace(r.Stderr + r.Stdout)})
	} else {
		rep.Checks = append(rep.Checks, ReadinessCheck{"sudo -n", "ok", "passwordless sudo works"})
	}

	// 2. bfb-install present.
	if r := runner.Run(ctx, "command -v bfb-install"); !r.OK() || strings.TrimSpace(r.Stdout) == "" {
		rep.Errors = append(rep.Errors, "bfb-install not found on PATH — install DOCA host package")
		rep.Checks = append(rep.Checks, ReadinessCheck{"bfb-install present", "fail", "missing"})
	} else {
		rep.Checks = append(rep.Checks, ReadinessCheck{"bfb-install present", "ok", strings.TrimSpace(r.Stdout)})
	}

	// 3. mst (MFT) present — needed by bfb_post_install for mlxconfig writes.
	if r := runner.Run(ctx, "command -v mst"); !r.OK() || strings.TrimSpace(r.Stdout) == "" {
		rep.Warnings = append(rep.Warnings, "mst (MFT) not found on PATH — bfb_post_install mlxconfig step will fail; install kernel-mft-dkms")
		rep.Checks = append(rep.Checks, ReadinessCheck{"mst present", "warn", "missing"})
	} else {
		rep.Checks = append(rep.Checks, ReadinessCheck{"mst present", "ok", strings.TrimSpace(r.Stdout)})
	}

	// 4. /dev/rshim0/boot writable (in principle) — check the device exists
	//    and isn't held by another process.
	if r := runner.Run(ctx, "ls /dev/rshim0/boot 2>&1"); !r.OK() {
		rep.Errors = append(rep.Errors, "/dev/rshim0/boot missing — rshim driver not loaded or DPU not enumerated")
		rep.Checks = append(rep.Checks, ReadinessCheck{"/dev/rshim0/boot", "fail", "missing"})
	} else {
		rep.Checks = append(rep.Checks, ReadinessCheck{"/dev/rshim0/boot", "ok", "present"})
		// Who's holding rshim?
		if r := runner.Run(ctx, "sudo -n fuser /dev/rshim0/boot 2>&1 | head -1"); r.OK() && strings.TrimSpace(r.Stdout) != "" {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("/dev/rshim0/boot is held by another process (%s) — flash may fail; release with `fuser -k /dev/rshim0/boot`", strings.TrimSpace(r.Stdout)))
			rep.Checks = append(rep.Checks, ReadinessCheck{"rshim free", "warn", strings.TrimSpace(r.Stdout)})
		} else {
			rep.Checks = append(rep.Checks, ReadinessCheck{"rshim free", "ok", "no holder"})
		}
	}

	// 5. NIC mode == DPU on this card (INTERNAL_CPU_MODEL == EMBEDDED_CPU).
	if dpuPCI != "" {
		cmd := fmt.Sprintf("sudo -n mlxconfig -d %s -e q 2>/dev/null | grep INTERNAL_CPU_MODEL || true", dpuPCI)
		r := runner.Run(ctx, cmd)
		if r.OK() && strings.Contains(r.Stdout, "EMBEDDED_CPU") {
			rep.Checks = append(rep.Checks, ReadinessCheck{"INTERNAL_CPU_MODEL", "ok", "EMBEDDED_CPU (DPU mode)"})
		} else if r.OK() && strings.Contains(r.Stdout, "SEPARATED_HOST") {
			rep.Errors = append(rep.Errors, fmt.Sprintf("DPU %s is in NIC mode (SEPARATED_HOST) — switch to EMBEDDED_CPU first via `dpubnkctl provision set-mode` (planned, or do it manually with mlxconfig + cold boot)", dpuPCI))
			rep.Checks = append(rep.Checks, ReadinessCheck{"INTERNAL_CPU_MODEL", "fail", "SEPARATED_HOST (NIC mode)"})
		} else {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("could not read INTERNAL_CPU_MODEL for %s — proceeding assumes DPU mode", dpuPCI))
			rep.Checks = append(rep.Checks, ReadinessCheck{"INTERNAL_CPU_MODEL", "warn", "unknown"})
		}
	}

	// 6. Disk space in /tmp for the BFB image (~2 GB needed).
	if r := runner.Run(ctx, "df -BM --output=avail /tmp | tail -1"); r.OK() {
		s := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(r.Stdout), "M"))
		var availMB int
		_, _ = fmt.Sscanf(s, "%d", &availMB)
		if availMB < 3000 {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("/tmp has %d MB free — BFB image is ~2 GB, push may fail", availMB))
			rep.Checks = append(rep.Checks, ReadinessCheck{"/tmp space", "warn", fmt.Sprintf("%d MB available", availMB)})
		} else {
			rep.Checks = append(rep.Checks, ReadinessCheck{"/tmp space", "ok", fmt.Sprintf("%d MB available", availMB)})
		}
	}

	return rep
}

// dialAsRunner is a tiny adapter so callers can pass an *ssh.Client to
// Check(), keeping the package's surface decoupled from x/crypto/ssh.
type dialAsRunner struct{ c *ssh.Client }

func (d dialAsRunner) Run(ctx context.Context, cmd string) ssh.Result { return d.c.Run(ctx, cmd) }

// AsRunner wraps an *ssh.Client into a discover.Runner for use with Check.
func AsRunner(c *ssh.Client) discover.Runner { return dialAsRunner{c: c} }
