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

// probeRshim detects rshim by either kernel module presence or /dev/rshim*
// device nodes. Some setups ship rshim as a built-in module (no lsmod
// row) but still expose /dev/rshim0 — treat that as "loaded".
func probeRshim(ctx context.Context, c Runner) RshimState {
	st := RshimState{}
	if r := c.Run(ctx, "lsmod 2>/dev/null | grep -Eq '^(mlx_)?rshim' && echo yes || echo no"); r.OK() {
		st.Loaded = strings.TrimSpace(r.Stdout) == "yes"
	}
	if r := c.Run(ctx, "ls -d /dev/rshim* 2>/dev/null || true"); r.OK() {
		for _, line := range strings.Fields(r.Stdout) {
			st.Devices = append(st.Devices, line)
		}
	}
	// /dev/rshim* without an lsmod row still counts as loaded.
	if !st.Loaded && len(st.Devices) > 0 {
		st.Loaded = true
	}
	return st
}

// probeDPUs lists all Mellanox/NVIDIA functions, then for each unique card
// (PCI domain:bus) attempts mlxconfig + rshim queries. The bool return is
// true when this host is itself a BlueField SoC running the DPU OS, not a
// server with a DPU attached — detected by PCI bridges in the 15b3:* set
// (see parseLspciDPUs).
func probeDPUs(ctx context.Context, c Runner, tools Tools) ([]DPUDetail, bool, []string) {
	var warnings []string

	r := c.Run(ctx, "lspci -nn -d 15b3: 2>/dev/null || true")
	if !r.OK() || strings.TrimSpace(r.Stdout) == "" {
		return nil, false, []string{"no Mellanox/NVIDIA devices on PCIe (lspci 15b3:* empty)"}
	}

	functions, looksLikeDPUOS := parseLspciDPUs(r.Stdout)
	if looksLikeDPUOS {
		// Don't run mlxconfig / rshim probes — the BF3's own OS doesn't
		// have those tools wired the same way as a server-side BSP.
		// Callers (range scan / wizard) use the IsDPU flag to exclude
		// this address from the host-candidate list.
		return functions, true, []string{"this host appears to be a BlueField DPU OS (PCI bridges in 15b3:*); skipping mlxconfig/rshim probes"}
	}
	dpus := mergeFunctionsByCard(functions)

	if tools.Mlxconfig == "" {
		warnings = append(warnings, "mlxconfig not installed: DPU NV-config not collected (install MFT to enable)")
	}

	for i := range dpus {
		if tools.Mlxconfig == "" || dpus[i].PCI == "" {
			continue
		}
		// mlxconfig needs root on every machine I've seen. Try direct first
		// (cheap if user is root); if it fails, fall back to passwordless
		// sudo. If both fail, warn — the operator may need NOPASSWD sudo.
		got := tryMlxconfig(ctx, c, dpus[i].PCI, "")
		if got == nil {
			got = tryMlxconfig(ctx, c, dpus[i].PCI, "sudo -n ")
		}
		if got == nil {
			warnings = append(warnings,
				fmt.Sprintf("mlxconfig present but could not read DPU %s (root or NOPASSWD sudo required)", dpus[i].PCI))
			continue
		}
		dpus[i].Mlxconfig = got
	}

	// rshim misc — if a single device, capture its misc once and assign to
	// the first DPU; mapping rshim<->PCI requires extra plumbing we'll do
	// in Phase 2 (where it actually matters for flashing). Try plain then
	// sudo -n; /dev/rshim0/misc is root-only on most systems.
	if misc := tryRshimMisc(ctx, c); misc != nil && len(dpus) > 0 {
		dpus[0].RshimMisc = misc
	}

	return dpus, false, warnings
}

func tryMlxconfig(ctx context.Context, c Runner, pci, prefix string) *DPUMlxconfig {
	cmd := fmt.Sprintf("%smlxconfig -d %s -e q 2>/dev/null || true", prefix, pci)
	r := c.Run(ctx, cmd)
	if !r.OK() || strings.TrimSpace(r.Stdout) == "" {
		return nil
	}
	if !strings.Contains(r.Stdout, "Configurations:") {
		return nil
	}
	return parseMlxconfig(r.Stdout)
}

func tryRshimMisc(ctx context.Context, c Runner) map[string]string {
	for _, prefix := range []string{"", "sudo -n "} {
		cmd := prefix + "cat /dev/rshim0/misc 2>/dev/null || true"
		r := c.Run(ctx, cmd)
		if !r.OK() || strings.TrimSpace(r.Stdout) == "" {
			continue
		}
		if misc := parseRshimMisc(r.Stdout); misc != nil {
			return misc
		}
	}
	return nil
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
