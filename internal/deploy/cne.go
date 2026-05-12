package deploy

import (
	"bytes"
	"fmt"
	"net"
	"strings"
	"text/template"

	"github.com/mwiget/dpubnkctl/internal/embedded"
	"github.com/mwiget/dpubnkctl/internal/poc"
	"github.com/mwiget/dpubnkctl/internal/version"
)

// CNEInputs is the flat shape passed to cne-instance.yaml.tmpl.
type CNEInputs struct {
	InstanceName       string
	ManifestVersion    string
	StorageClass       string
	DPUEnabled         bool
	DeploymentSize     string
	NetworkAttachments []string
	DPUMtu             int
}

// VLANInputs is one F5SPKVlan entry. Aggregated from all DPUs by tag —
// each VLAN has multiple selfip_v4s (one per DPU's IP in that subnet).
type VLANInputs struct {
	Name      string
	MTU       int
	Tag       int
	Interface string   // TMM-side interface (e.g., "1.1", "1.2")
	SelfIPv4s []string // one per DPU
	PrefixV4  int
}

// GatewayClassInputs feeds bnk-gatewayclass.yaml.tmpl.
type GatewayClassInputs struct {
	GatewayClassName string
	TMMReplicas      int
}

// RenderCNEInstance picks defaults sensible for the lake1-style PoC:
// dpu_enabled if any DPU exists, deploymentSize "Large" for ≥2 TMMs,
// "Small" otherwise. NetworkAttachments hard-coded to sf-external/
// sf-internal (operator can override later via poc.yaml.bnk).
func RenderCNEInstance(p *poc.PoC) (string, error) {
	dpuCount := 0
	for _, h := range p.Hosts {
		dpuCount += len(h.DPUs)
	}
	deployment := "Small"
	if dpuCount >= 2 {
		deployment = "Large"
	}
	mtu := p.Network.DPUMTU
	if mtu == 0 {
		mtu = version.DefaultDPUMTU
	}
	in := CNEInputs{
		InstanceName:       "bnk-instance",
		ManifestVersion:    version.CNEManifestVersion,
		StorageClass:       "local-path",
		DPUEnabled:         dpuCount > 0,
		DeploymentSize:     deployment,
		NetworkAttachments: []string{"sf-external", "sf-internal"},
		DPUMtu:             mtu,
	}
	return renderTemplate("templates/cne-instance.yaml.tmpl", in)
}

// RenderF5SPKVlans aggregates poc.yaml.hosts[].dpus[].vlans by tag (so
// a logical "external" VLAN with tag=40 across multiple DPUs gets ONE
// F5SPKVlan with multiple selfip_v4s). The TMM-side interface name
// follows the f5-bnk convention: 1.1 for the first VLAN, 1.2 for the
// second, etc. — matches the order they appear in the first DPU.
func RenderF5SPKVlans(p *poc.PoC) (string, error) {
	type aggKey struct{ name string }
	type aggVal struct {
		tag       int
		ipsByHost []string
		prefix    int
	}
	agg := map[aggKey]*aggVal{}
	order := []aggKey{}
	for _, h := range p.Hosts {
		for _, d := range h.DPUs {
			for _, v := range d.VLANs {
				key := aggKey{name: v.PortName()}
				if _, exists := agg[key]; !exists {
					agg[key] = &aggVal{tag: v.Tag}
					order = append(order, key)
				}
				ip, ipnet, err := net.ParseCIDR(v.IP)
				if err != nil {
					return "", fmt.Errorf("bad ip %q on %s/%s: %w", v.IP, h.Name, v.PortName(), err)
				}
				agg[key].ipsByHost = append(agg[key].ipsByHost, ip.String())
				ones, _ := ipnet.Mask.Size()
				agg[key].prefix = ones
			}
		}
	}

	in := struct {
		VLANs []VLANInputs
	}{}
	for i, k := range order {
		v := agg[k]
		in.VLANs = append(in.VLANs, VLANInputs{
			Name:      k.name,
			MTU:       p.Network.DPUMTU,
			Tag:       v.tag,
			Interface: fmt.Sprintf("1.%d", i+1),
			SelfIPv4s: v.ipsByHost,
			PrefixV4:  v.prefix,
		})
	}
	return renderTemplate("templates/f5spkvlan.yaml.tmpl", in)
}

// RenderGatewayClass returns the BNKGatewayClassConfig + GatewayClass
// manifest pair. tmmReplicas defaults to 1; for multi-DPU clusters
// callers may want higher.
func RenderGatewayClass(name string, tmmReplicas int) (string, error) {
	if name == "" {
		name = "bnk-gatewayclass"
	}
	if tmmReplicas == 0 {
		tmmReplicas = 1
	}
	return renderTemplate("templates/bnk-gatewayclass.yaml.tmpl", GatewayClassInputs{
		GatewayClassName: name,
		TMMReplicas:      tmmReplicas,
	})
}

func renderTemplate(path string, data any) (string, error) {
	raw, err := embedded.Templates.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("load %s: %w", path, err)
	}
	tmpl, err := template.New(path).Parse(string(raw))
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	if err := tmpl.Execute(&out, data); err != nil {
		return "", fmt.Errorf("execute %s: %w", path, err)
	}
	return out.String(), nil
}

// allHostsVLANCount is a small util for tests + callers that want a
// quick count without re-aggregating.
func allHostsVLANCount(p *poc.PoC) int {
	seen := map[string]bool{}
	for _, h := range p.Hosts {
		for _, d := range h.DPUs {
			for _, v := range d.VLANs {
				seen[v.PortName()] = true
			}
		}
	}
	return len(seen)
}

// dpuCount returns the total number of DPUs across all hosts.
func dpuCount(p *poc.PoC) int {
	n := 0
	for _, h := range p.Hosts {
		n += len(h.DPUs)
	}
	return n
}

var _ = strings.Builder{} // keep strings import for future use
