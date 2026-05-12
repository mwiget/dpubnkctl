package discover

import (
	"context"
	"time"
)

// HostOptions configures a single-host discovery run.
type HostOptions struct {
	Address string // user-facing identifier (IP or hostname)
	Runner  Runner // satisfied by *ssh.Client (real) or fake (test)
}

// DiscoverHost runs the full probe battery against one host. It never
// returns a hard error from a single failed probe — partial results are
// returned with warnings recorded.
func DiscoverHost(ctx context.Context, opts HostOptions) (*Result, error) {
	r := &Result{
		Address:      opts.Address,
		DiscoveredAt: time.Now().UTC(),
	}

	host, hostWarn := probeHostInfo(ctx, opts.Runner)
	r.Host = host
	r.Warnings = append(r.Warnings, hostWarn...)

	if host.Tools.Ipmitool != "" {
		bmc, bmcWarn := probeBMC(ctx, opts.Runner)
		r.BMC = bmc
		r.Warnings = append(r.Warnings, bmcWarn...)
	} else {
		r.Warnings = append(r.Warnings, "ipmitool not installed: BMC not auto-discovered")
	}

	dpus, dpuWarn := probeDPUs(ctx, opts.Runner, host.Tools)
	r.DPUs = dpus
	r.Warnings = append(r.Warnings, dpuWarn...)

	return r, nil
}

// Classification is a one-line summary the CLI prints after discovery.
func (r *Result) Classification() string {
	switch {
	case len(r.DPUs) == 0:
		return "host without DPU"
	case len(r.DPUs) == 1:
		return "host with 1 DPU"
	default:
		return "host with multiple DPUs"
	}
}
