package cli

import (
	"strings"
	"testing"

	"github.com/mwiget/dpubnkctl/internal/poc"
)

// TestRenderHostNetplan_ParentMTUMatchesVLAN exercises the fix for the
// ailab single-node PoC: cloud-init's 50-cloud-init.yaml sets
// enp3s0f0np0 to MTU 1500, our 70-dpubnkctl-dataplane.yaml asks for
// VLAN sub-ifs at MTU 9000. netplan deep-merges across files, so unless
// 70-* also defines an `ethernets:` block bumping the parent MTU, the
// kernel either rejects the stacked netdev (`Could not create stacked
// netdev: Invalid argument`) or silently clamps the VLAN sub-ifs down
// to 1500 — both fail TMM jumbo traffic later.
func TestRenderHostNetplan_ParentMTUMatchesVLAN(t *testing.T) {
	vlans := []poc.HostDataPlaneVLAN{
		{Role: "external", Tag: 40, IP: "10.40.21.21/24"},
		{Role: "internal", Tag: 50, IP: "10.50.21.21/24"},
	}
	out := renderHostNetplan(&poc.HostDataPlane{ParentIface: "enp3s0f0np0", VLANs: vlans}, 9000)

	// Must include an ethernets block for the parent at MTU 9000.
	if !strings.Contains(out, "ethernets:") {
		t.Fatalf("expected an `ethernets:` block to override parent MTU; got:\n%s", out)
	}
	if !strings.Contains(out, "enp3s0f0np0:\n      mtu: 9000") {
		t.Errorf("expected parent_iface enp3s0f0np0 with mtu: 9000; got:\n%s", out)
	}

	// And the VLAN sub-interfaces at the same MTU.
	for _, want := range []string{"external40:", "id: 40", "internal50:", "id: 50", "mtu: 9000"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered netplan missing %q; got:\n%s", want, out)
		}
	}
}

// TestRenderHostNetplan_ParentMTUTakesMaxOfChildren handles the case
// where individual VLANs override defaultMTU. Parent must be the max
// of all child MTUs so no child gets clamped.
func TestRenderHostNetplan_ParentMTUTakesMaxOfChildren(t *testing.T) {
	vlans := []poc.HostDataPlaneVLAN{
		{Role: "external", Tag: 40, IP: "10.40.0.1/24", MTU: 1500}, // unusual, but allowed
		{Role: "storage", Tag: 60, IP: "10.60.0.1/24", MTU: 9216},  // jumbo
	}
	out := renderHostNetplan(&poc.HostDataPlane{ParentIface: "ens16f0np0", VLANs: vlans}, 9000)
	if !strings.Contains(out, "ens16f0np0:\n      mtu: 9216") {
		t.Errorf("parent MTU should be the max child MTU (9216), not the default (9000); got:\n%s", out)
	}
}

// TestRenderHostNetplan_PerVLANMTUOverride pins per-VLAN MTU override
// behaviour — explicit MTU on a VLAN entry wins over network.dpu_mtu.
func TestRenderHostNetplan_PerVLANMTUOverride(t *testing.T) {
	vlans := []poc.HostDataPlaneVLAN{
		{Role: "external", Tag: 40, IP: "10.40.0.1/24"},          // → defaultMTU
		{Role: "mgmt", Tag: 100, IP: "10.100.0.1/24", MTU: 1500}, // explicit
	}
	out := renderHostNetplan(&poc.HostDataPlane{ParentIface: "eno1", VLANs: vlans}, 9000)
	if !strings.Contains(out, "external40:\n      id: 40\n      link: eno1\n      mtu: 9000") {
		t.Errorf("expected external40 at defaultMTU 9000; got:\n%s", out)
	}
	if !strings.Contains(out, "mgmt100:\n      id: 100\n      link: eno1\n      mtu: 1500") {
		t.Errorf("expected mgmt100 at explicit MTU 1500; got:\n%s", out)
	}
}

// TestRenderHostNetplan_PerVLANParentIface is the non-LAG fix (issue #18,
// Tokyo lab). A non-LAG DPU has two OVS bridges fed by two host PFs, so
// the internal VLAN must hang off PF1 while external stays on PF0.
// Previously every VLAN was pinned to the single block-level parent,
// which delivered VLAN 50 to sf-external and stranded it there.
func TestRenderHostNetplan_PerVLANParentIface(t *testing.T) {
	dp := &poc.HostDataPlane{
		ParentIface: "enp13s0f0np0",
		VLANs: []poc.HostDataPlaneVLAN{
			{Role: "external", Tag: 40, IP: "10.40.0.1/24"},
			{Role: "internal", Tag: 50, IP: "10.50.0.1/24", ParentIface: "enp13s0f1np1"},
		},
	}
	out := renderHostNetplan(dp, 9000)

	if !strings.Contains(out, "external40:\n      id: 40\n      link: enp13s0f0np0") {
		t.Errorf("external40 should inherit the block-level parent; got:\n%s", out)
	}
	if !strings.Contains(out, "internal50:\n      id: 50\n      link: enp13s0f1np1") {
		t.Errorf("internal50 should use its per-VLAN parent_iface override; got:\n%s", out)
	}
	// Both PFs need an ethernets stanza, or netplan leaves PF1 at the
	// cloud-init MTU and the kernel clamps internal50 down with it.
	for _, want := range []string{"    enp13s0f0np0:\n      mtu: 9000", "    enp13s0f1np1:\n      mtu: 9000"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected an ethernets stanza %q; got:\n%s", want, out)
		}
	}
}

// TestRenderHostNetplan_PerParentMTUIsIndependent pins that each parent
// takes the max of *its own* children, not of every VLAN on the host —
// PF1 carrying only a 1500 VLAN shouldn't be dragged to 9216 by a jumbo
// VLAN that lives on PF0.
func TestRenderHostNetplan_PerParentMTUIsIndependent(t *testing.T) {
	dp := &poc.HostDataPlane{
		ParentIface: "pf0",
		VLANs: []poc.HostDataPlaneVLAN{
			{Role: "storage", Tag: 60, IP: "10.60.0.1/24", MTU: 9216},
			{Role: "mgmt", Tag: 100, IP: "10.100.0.1/24", MTU: 1500, ParentIface: "pf1"},
		},
	}
	out := renderHostNetplan(dp, 1500)
	if !strings.Contains(out, "    pf0:\n      mtu: 9216") {
		t.Errorf("pf0 should take its jumbo child's MTU; got:\n%s", out)
	}
	if !strings.Contains(out, "    pf1:\n      mtu: 1500") {
		t.Errorf("pf1 should stay at 1500, not inherit pf0's 9216; got:\n%s", out)
	}
}
