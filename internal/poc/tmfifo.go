package poc

import (
	"fmt"
	"net"
)

// tmfifo addressing for rshim joins.
//
// Each host↔DPU tmfifo link is a separate point-to-point segment that
// needs the host and the DPU in the same /30 to talk. Single-host rshim
// reuses the rshim driver default 192.168.100.1/.2 /30. Multi-host rshim
// (network.tmfifo_cidr set) carves a *unique* /30 per DPU from the pool
// so the kubelet --node-ip (= the DPU's tmfifo IP) is cluster-unique and
// no two hosts collide on 192.168.100.x — the root of the historic
// "dup tmfifo" scramble.
//
// Allocation is deterministic by (host index, dpu index): the k-th DPU
// in the fleet gets the k-th /30 of the pool, host side = .1, DPU side
// = .2. Re-running is idempotent and yields the same addresses, so a
// redeploy that re-reads poc.yaml doesn't renumber a running cluster.

// AllocateTmfifo assigns host-side and DPU-side tmfifo addresses for an
// rshim join and writes them back into p (host.tmfifo_ip, dpu.tmfifo_ip).
// It reports whether anything changed so the caller can decide to persist
// poc.yaml.
//
// Behavior by configuration:
//   - not rshim: no-op (the vlan transport doesn't touch tmfifo here).
//   - rshim, no tmfifo_cidr: single-host default — sets the (single) host's
//     tmfifo_ip to 192.168.100.1/30 and its DPU's tmfifo_ip to
//     192.168.100.2/30 if unset. Refuses to default-address more than one
//     host (that needs a pool).
//   - rshim, tmfifo_cidr set: carves a /30 per DPU from the pool.
func AllocateTmfifo(p *PoC) (changed bool, err error) {
	if !p.Network.IsRshim() {
		return false, nil
	}

	if p.Network.TmfifoCIDR == "" {
		return allocateTmfifoSingleHost(p)
	}
	return allocateTmfifoFromPool(p)
}

// allocateTmfifoSingleHost handles the poolless default: exactly one host
// on the rshim driver's built-in 192.168.100.1/.2 /30.
func allocateTmfifoSingleHost(p *PoC) (bool, error) {
	hostsWithDPUs := 0
	for i := range p.Hosts {
		if len(p.Hosts[i].DPUs) > 0 {
			hostsWithDPUs++
		}
	}
	if hostsWithDPUs > 1 {
		return false, fmt.Errorf("join_transport=rshim with %d hosts needs network.tmfifo_cidr — the rshim default 192.168.100.x /30 only works for a single host", hostsWithDPUs)
	}

	changed := false
	for i := range p.Hosts {
		h := &p.Hosts[i]
		if len(h.DPUs) == 0 {
			continue
		}
		if len(h.DPUs) > 1 {
			return false, fmt.Errorf("host %s has %d DPUs on rshim without network.tmfifo_cidr — one tmfifo /30 can address only one DPU; set tmfifo_cidr", h.Name, len(h.DPUs))
		}
		if h.TmfifoIP != DefaultTmfifoHostIP {
			h.TmfifoIP = DefaultTmfifoHostIP
			changed = true
		}
		if h.DPUs[0].TmfifoIP != DefaultTmfifoDPUIP {
			h.DPUs[0].TmfifoIP = DefaultTmfifoDPUIP
			changed = true
		}
	}
	return changed, nil
}

// allocateTmfifoFromPool carves a unique /30 per DPU out of tmfifo_cidr.
func allocateTmfifoFromPool(p *PoC) (bool, error) {
	_, pool, err := net.ParseCIDR(p.Network.TmfifoCIDR)
	if err != nil {
		return false, fmt.Errorf("network.tmfifo_cidr %q is not a valid CIDR", p.Network.TmfifoCIDR)
	}
	poolOnes, _ := pool.Mask.Size()
	// A /30 per DPU: the pool must be at least /30, and its size caps the
	// number of DPUs it can hold.
	if poolOnes > 30 {
		return false, fmt.Errorf("network.tmfifo_cidr %q is smaller than a /30 — cannot carve any DPU link from it", p.Network.TmfifoCIDR)
	}
	capacity := 1 << uint(30-poolOnes) // number of /30 blocks in the pool
	base := ipToU32(pool.IP)

	changed := false
	k := 0
	for i := range p.Hosts {
		h := &p.Hosts[i]
		for j := range h.DPUs {
			if k >= capacity {
				return false, fmt.Errorf("network.tmfifo_cidr %q holds %d /30 blocks but the fleet has more DPUs — widen the pool", p.Network.TmfifoCIDR, capacity)
			}
			blockNet := base + uint32(k)*4 // /30-aligned network address
			hostIP := fmt.Sprintf("%s/30", u32ToIP(blockNet+1))
			dpuIP := fmt.Sprintf("%s/30", u32ToIP(blockNet+2))

			if h.DPUs[j].TmfifoIP != dpuIP {
				h.DPUs[j].TmfifoIP = dpuIP
				changed = true
			}
			// The host-side address tracks its first DPU's /30. Multi-DPU
			// hosts over rshim aren't supported yet (one tmfifo_net0 per
			// host); validate rejects that combination.
			if j == 0 && h.TmfifoIP != hostIP {
				h.TmfifoIP = hostIP
				changed = true
			}
			k++
		}
	}
	return changed, nil
}

func ipToU32(ip net.IP) uint32 {
	ip4 := ip.To4()
	if ip4 == nil {
		return 0
	}
	return uint32(ip4[0])<<24 | uint32(ip4[1])<<16 | uint32(ip4[2])<<8 | uint32(ip4[3])
}

func u32ToIP(v uint32) net.IP {
	return net.IPv4(byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}
