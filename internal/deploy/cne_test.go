package deploy

import (
	"strings"
	"testing"

	"github.com/mwiget/dpubnkctl/internal/poc"
)

func cneFixture() *poc.PoC {
	p := poc.New("test")
	p.Network.DPUMTU = 9000
	p.Hosts = []poc.Host{
		{
			Name: "worker1",
			DPUs: []poc.DPU{{
				PCI:      "00:10.0",
				Hostname: "worker1-bf3",
				LAG:      true,
				VLANs: []poc.DPUVLAN{
					{Role: "external", Tag: 40, IP: "10.10.40.5/24"},
					{Role: "internal", Tag: 41, IP: "10.10.41.5/24"},
				},
			}},
		},
		{
			Name: "worker2",
			DPUs: []poc.DPU{{
				PCI:      "00:10.0",
				Hostname: "worker2-bf3",
				LAG:      true,
				VLANs: []poc.DPUVLAN{
					{Role: "external", Tag: 40, IP: "10.10.40.6/24"},
					{Role: "internal", Tag: 41, IP: "10.10.41.6/24"},
				},
			}},
		},
	}
	return p
}

func TestRenderCNEInstance_DPUEnabled(t *testing.T) {
	p := cneFixture()
	out, err := RenderCNEInstance(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"kind: CNEInstance",
		`manifestVersion: "2.2.0-3.2226.0-0.0.385"`,
		"enabled: true", // dpu.enabled
		`deploymentSize: "Large"`, // 2 DPUs → Large
		"- sf-external",
		"- sf-internal",
		`value: "9000"`, // TMM_DEFAULT_MTU
		"clusterIssuer: bnk-ca-cluster-issuer",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("CNEInstance missing %q", want)
		}
	}
}

func TestRenderF5SPKVlans_AggregatesByName(t *testing.T) {
	p := cneFixture()
	out, err := RenderF5SPKVlans(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"kind: F5SPKVlan",
		"name: external40",
		"name: internal41",
		"tag: 40",
		"tag: 41",
		`- "1.1"`, // external40 → 1.1
		`- "1.2"`, // internal41 → 1.2
		"- 10.10.40.5", // worker1's external IP
		"- 10.10.40.6", // worker2's external IP
		"- 10.10.41.5",
		"- 10.10.41.6",
		"prefixlen_v4: 24",
		`auto_lasthop: "AUTO_LASTHOP_ENABLED"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("F5SPKVlans missing %q\n%s", want, out)
		}
	}
}

func TestRenderGatewayClass(t *testing.T) {
	out, err := RenderGatewayClass("bnk-gatewayclass")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"apiVersion: gateway.networking.k8s.io/v1",
		"kind: GatewayClass",
		"name: bnk-gatewayclass",
		"controllerName: f5.com/default-f5-cne-controller",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("GatewayClass missing %q in:\n%s", want, out)
		}
	}
	// Defensive: the historical BNKGatewayClassConfig CRD doesn't exist
	// in BNK 2.2.0 — make sure no future template edit re-introduces it.
	for _, banned := range []string{
		"BNKGatewayClassConfig",
		"gateway.f5.com",
		"defaultTmmReplicas",
	} {
		if strings.Contains(out, banned) {
			t.Errorf("GatewayClass should NOT contain %q (CRD doesn't exist on BNK 2.2.0):\n%s", banned, out)
		}
	}
}

func TestRenderGatewayClass_DefaultName(t *testing.T) {
	out, err := RenderGatewayClass("")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "name: bnk-gatewayclass") {
		t.Errorf("empty name should default to bnk-gatewayclass, got:\n%s", out)
	}
}
