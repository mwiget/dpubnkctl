package poc

import "testing"

func hostWithDPUs(name string, n int) Host {
	h := Host{Name: name, Role: "both"}
	for i := 0; i < n; i++ {
		h.DPUs = append(h.DPUs, DPU{PCI: "0000:03:00.0"})
	}
	return h
}

func TestAllocateTmfifo_NotRshim(t *testing.T) {
	p := &PoC{Network: Network{JoinTransport: JoinTransportVLAN}, Hosts: []Host{hostWithDPUs("h1", 1)}}
	changed, err := AllocateTmfifo(p)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if changed {
		t.Fatalf("vlan transport should be a no-op")
	}
	if p.Hosts[0].DPUs[0].TmfifoIP != "" {
		t.Fatalf("vlan transport must not set tmfifo IPs, got %q", p.Hosts[0].DPUs[0].TmfifoIP)
	}
}

func TestAllocateTmfifo_SingleHostDefault(t *testing.T) {
	p := &PoC{Network: Network{JoinTransport: JoinTransportRshim}, Hosts: []Host{hostWithDPUs("h1", 1)}}
	changed, err := AllocateTmfifo(p)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !changed {
		t.Fatalf("expected changed=true on first allocation")
	}
	if got := p.Hosts[0].TmfifoIP; got != DefaultTmfifoHostIP {
		t.Errorf("host tmfifo = %q, want %q", got, DefaultTmfifoHostIP)
	}
	if got := p.Hosts[0].DPUs[0].TmfifoIP; got != DefaultTmfifoDPUIP {
		t.Errorf("dpu tmfifo = %q, want %q", got, DefaultTmfifoDPUIP)
	}
	// Idempotent: second run makes no change.
	if changed, _ := AllocateTmfifo(p); changed {
		t.Errorf("second allocation should be a no-op")
	}
}

func TestAllocateTmfifo_SingleHostMultipleHostsNeedsPool(t *testing.T) {
	p := &PoC{Network: Network{JoinTransport: JoinTransportRshim}, Hosts: []Host{hostWithDPUs("h1", 1), hostWithDPUs("h2", 1)}}
	if _, err := AllocateTmfifo(p); err == nil {
		t.Fatalf("expected error for 2 hosts without tmfifo_cidr")
	}
}

func TestAllocateTmfifo_FromPool(t *testing.T) {
	p := &PoC{
		Network: Network{JoinTransport: JoinTransportRshim, TmfifoCIDR: "192.168.0.0/24"},
		Hosts:   []Host{hostWithDPUs("h1", 1), hostWithDPUs("h2", 1)},
	}
	if _, err := AllocateTmfifo(p); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	// /30 #0 = 192.168.0.0 → host .1, dpu .2; #1 = 192.168.0.4 → .5/.6.
	checks := []struct {
		host, dpu string
	}{
		{"192.168.0.1/30", "192.168.0.2/30"},
		{"192.168.0.5/30", "192.168.0.6/30"},
	}
	for i, want := range checks {
		if got := p.Hosts[i].TmfifoIP; got != want.host {
			t.Errorf("host[%d] tmfifo = %q, want %q", i, got, want.host)
		}
		if got := p.Hosts[i].DPUs[0].TmfifoIP; got != want.dpu {
			t.Errorf("host[%d] dpu tmfifo = %q, want %q", i, got, want.dpu)
		}
	}
	// Deterministic + idempotent.
	if changed, _ := AllocateTmfifo(p); changed {
		t.Errorf("re-allocation from pool should be a no-op")
	}
}

func TestAllocateTmfifo_PoolTooSmall(t *testing.T) {
	// /31 pool holds zero /30 blocks.
	p := &PoC{
		Network: Network{JoinTransport: JoinTransportRshim, TmfifoCIDR: "192.168.0.0/31"},
		Hosts:   []Host{hostWithDPUs("h1", 1)},
	}
	if _, err := AllocateTmfifo(p); err == nil {
		t.Fatalf("expected error for pool smaller than /30")
	}
}

func TestAllocateTmfifo_PoolExhausted(t *testing.T) {
	// /30 pool holds exactly one /30 → one DPU; two DPUs overflow.
	p := &PoC{
		Network: Network{JoinTransport: JoinTransportRshim, TmfifoCIDR: "192.168.0.0/30"},
		Hosts:   []Host{hostWithDPUs("h1", 1), hostWithDPUs("h2", 1)},
	}
	if _, err := AllocateTmfifo(p); err == nil {
		t.Fatalf("expected error when fleet exceeds pool capacity")
	}
}

func TestEffectiveDPUInternet(t *testing.T) {
	cases := []struct {
		transport string
		explicit  string
		want      string
	}{
		{JoinTransportRshim, "", DPUInternetHostNAT},
		{JoinTransportVLAN, "", DPUInternetNone},
		{JoinTransportRshim, DPUInternetOOB, DPUInternetOOB},
		{JoinTransportVLAN, DPUInternetHostNAT, DPUInternetHostNAT},
	}
	for _, c := range cases {
		p := &PoC{Network: Network{JoinTransport: c.transport}, Provisioning: Provisioning{DPUInternet: c.explicit}}
		if got := p.EffectiveDPUInternet(); got != c.want {
			t.Errorf("transport=%q explicit=%q: got %q want %q", c.transport, c.explicit, got, c.want)
		}
	}
}
