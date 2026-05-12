package discover

import (
	"context"
	"fmt"
	"strings"

	"github.com/mwiget/dpubnkctl/internal/ssh"
)

// Runner is the subset of *ssh.Client probes need. The real client and
// test fakes both satisfy it.
type Runner interface {
	Run(ctx context.Context, cmd string) ssh.Result
}

// probeHostInfo collects kernel, OS, hostname, model, interfaces, tools.
func probeHostInfo(ctx context.Context, c Runner) (HostInfo, []string) {
	var warnings []string
	h := HostInfo{}

	if r := c.Run(ctx, "uname -r"); r.OK() {
		h.Kernel = strings.TrimSpace(r.Stdout)
	} else {
		warnings = append(warnings, "uname -r failed")
	}

	if r := c.Run(ctx, "hostname"); r.OK() {
		h.Hostname = strings.TrimSpace(r.Stdout)
	}

	if r := c.Run(ctx, "cat /etc/os-release"); r.OK() {
		h.OS = parseOSRelease(r.Stdout)
	}

	// dmidecode often requires root; treat absence as informational.
	if r := c.Run(ctx, "dmidecode -s system-product-name 2>/dev/null"); r.OK() {
		h.Model = strings.TrimSpace(r.Stdout)
	}

	if r := c.Run(ctx, "ip -j addr"); r.OK() {
		h.Interfaces = parseIPAddrJSON(r.Stdout)
	}

	h.Tools = probeTools(ctx, c)
	h.Rshim = probeRshim(ctx, c)
	return h, warnings
}

// probeTools records the path of each discovery-relevant binary, or "".
// `command -v` is portable across bash/sh and exits 0 only when found.
func probeTools(ctx context.Context, c Runner) Tools {
	t := Tools{}
	for _, x := range []struct {
		name string
		dst  *string
	}{
		{"mlxconfig", &t.Mlxconfig},
		{"bfb-install", &t.BFBInstall},
		{"ipmitool", &t.Ipmitool},
		{"rshim", &t.Rshim},
		{"mst", &t.Mst},
	} {
		r := c.Run(ctx, "command -v "+x.name+" 2>/dev/null || true")
		if r.OK() {
			*x.dst = strings.TrimSpace(r.Stdout)
		}
	}
	return t
}

// probeRshim checks for kernel module + /dev/rshim* devices.
func probeRshim(ctx context.Context, c Runner) RshimState {
	st := RshimState{}
	if r := c.Run(ctx, "lsmod 2>/dev/null | grep -q '^rshim ' && echo yes || echo no"); r.OK() {
		st.Loaded = strings.TrimSpace(r.Stdout) == "yes"
	}
	if r := c.Run(ctx, "ls -d /dev/rshim* 2>/dev/null || true"); r.OK() {
		for _, line := range strings.Fields(r.Stdout) {
			st.Devices = append(st.Devices, line)
		}
	}
	return st
}

// probeBMC tries `ipmitool lan print 1` then `ipmitool lan print` if the
// first returns nothing useful.
func probeBMC(ctx context.Context, c Runner) (*BMCInfo, []string) {
	for _, cmd := range []string{
		"ipmitool lan print 1 2>/dev/null",
		"ipmitool lan print 2>/dev/null",
	} {
		r := c.Run(ctx, cmd)
		if !r.OK() {
			continue
		}
		if bmc := parseIpmitoolLan(r.Stdout); bmc != nil {
			return bmc, nil
		}
	}
	return nil, []string{"BMC not auto-discovered (ipmitool absent or returned no IP)"}
}

// probeDPUs lists all Mellanox/NVIDIA functions, then for each unique card
// (PCI domain:bus) attempts mlxconfig + rshim queries.
func probeDPUs(ctx context.Context, c Runner, tools Tools) ([]DPUDetail, []string) {
	var warnings []string

	r := c.Run(ctx, "lspci -nn -d 15b3: 2>/dev/null || true")
	if !r.OK() || strings.TrimSpace(r.Stdout) == "" {
		return nil, []string{"no Mellanox/NVIDIA devices on PCIe (lspci 15b3:* empty)"}
	}

	functions := parseLspciDPUs(r.Stdout)
	dpus := mergeFunctionsByCard(functions)

	if tools.Mlxconfig == "" {
		warnings = append(warnings, "mlxconfig not installed: DPU NV-config not collected (install MFT to enable)")
	}

	for i := range dpus {
		// rshim/misc is only meaningful for DPUs the host can see via rshim.
		// We try the first /dev/rshim* — multi-DPU mapping comes later.
		if dpus[i].PCI != "" {
			if tools.Mlxconfig != "" {
				cmd := fmt.Sprintf("mlxconfig -d %s -e q 2>/dev/null || true", dpus[i].PCI)
				if rr := c.Run(ctx, cmd); rr.OK() && strings.TrimSpace(rr.Stdout) != "" {
					dpus[i].Mlxconfig = parseMlxconfig(rr.Stdout)
				}
			}
		}
	}

	// rshim misc — if a single device, capture its misc once and assign to
	// the first DPU; mapping rshim<->PCI requires extra plumbing we'll do
	// in Phase 2 (where it actually matters for flashing).
	if r := c.Run(ctx, "cat /dev/rshim0/misc 2>/dev/null || true"); r.OK() {
		if misc := parseRshimMisc(r.Stdout); misc != nil && len(dpus) > 0 {
			dpus[0].RshimMisc = misc
		}
	}

	return dpus, warnings
}

// mergeFunctionsByCard collapses lspci output to one entry per physical card.
// BlueField exposes pf0 + pf1 (e.g. 03:00.0 and 03:00.1); we keep the .0
// function and discard duplicates by domain:bus.
func mergeFunctionsByCard(fns []DPUDetail) []DPUDetail {
	seen := map[string]bool{}
	var out []DPUDetail
	for _, f := range fns {
		bus := f.PCI
		if i := strings.IndexByte(bus, '.'); i > 0 {
			bus = bus[:i]
		}
		if seen[bus] {
			continue
		}
		seen[bus] = true
		out = append(out, f)
	}
	return out
}
