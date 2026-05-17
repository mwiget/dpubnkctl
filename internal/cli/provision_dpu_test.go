package cli

import (
	"testing"

	"github.com/mwiget/dpubnkctl/internal/poc"
)

// TestDPUProbeAddrs documents the contract used by the post-flash boot
// waits: tmfifo first (rshim/PCIe path), oob second (independent of
// rshim's host-side .1/30 state). Two-target probing is what keeps the
// wait honest when an operator restarts rshim mid-flow — see the
// ailab single-node PoC for the false-negative this fix prevents.
func TestDPUProbeAddrs(t *testing.T) {
	cases := []struct {
		name string
		dpu  *poc.DPU
		want []string
	}{
		{
			name: "nil DPU → no addresses",
			dpu:  nil,
			want: nil,
		},
		{
			name: "neither set → no addresses",
			dpu:  &poc.DPU{},
			want: nil,
		},
		{
			name: "tmfifo only → tmfifo address",
			dpu:  &poc.DPU{TmfifoIP: "192.168.100.2/30"},
			want: []string{"192.168.100.2"},
		},
		{
			name: "oob only → oob address",
			dpu:  &poc.DPU{OOBIP: "10.10.10.140/22"},
			want: []string{"10.10.10.140"},
		},
		{
			name: "both → tmfifo first, oob second",
			dpu:  &poc.DPU{TmfifoIP: "192.168.100.2/30", OOBIP: "10.10.10.140/22"},
			want: []string{"192.168.100.2", "10.10.10.140"},
		},
		{
			name: "non-CIDR shapes still work (kept for back-compat)",
			dpu:  &poc.DPU{TmfifoIP: "192.168.100.2", OOBIP: "10.10.10.140"},
			want: []string{"192.168.100.2", "10.10.10.140"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := dpuProbeAddrs(tc.dpu)
			if !sameSlice(got, tc.want) {
				t.Errorf("dpuProbeAddrs() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestTmfifoHostPart pins the CIDR-strip behaviour the probe-address
// builder relies on. It's already used by both tmfifo_ip and oob_ip,
// so its inputs are wider than the name suggests.
func TestTmfifoHostPart(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"192.168.100.2/30", "192.168.100.2"},
		{"10.10.10.140/22", "10.10.10.140"},
		{"10.10.10.140", "10.10.10.140"},
		{"/24", "/24"}, // pathological: leading slash → no host part, return as-is
	}
	for _, tc := range cases {
		if got := tmfifoHostPart(tc.in); got != tc.want {
			t.Errorf("tmfifoHostPart(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func sameSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
