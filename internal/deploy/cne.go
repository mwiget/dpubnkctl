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
	ImagePullPolicy    string
	PodCIDR            string
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
}

// RenderCNEInstance picks defaults sensible for the lake1-style PoC:
// dpu_enabled if any DPU exists, deploymentSize "Large" for ≥2 TMMs,
// "Small" otherwise. NetworkAttachments hard-coded to sf-external/
// sf-internal (operator can override later via poc.yaml.bnk).
func RenderCNEInstance(p *poc.PoC) (string, error) {
	mtu := p.Network.DPUMTU
	if mtu == 0 {
		mtu = version.DefaultDPUMTU
	}
	pullPolicy := "Always"
	if p.Airgap != nil && p.Airgap.Mode != "" {
		pullPolicy = "IfNotPresent"
	}
	storageClass := "nfs"
	podCIDR := "10.233.64.0/18"
	in := CNEInputs{
		InstanceName:       "bnk-instance",
		ManifestVersion:    version.CNEManifestVersion,
		StorageClass:       storageClass,
		DPUEnabled:         dpuCount(p) > 0,
		DeploymentSize:     "Large",
		NetworkAttachments: []string{"sf-external", "sf-internal"},
		DPUMtu:             mtu,
		ImagePullPolicy:    pullPolicy,
		PodCIDR:            podCIDR,
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

// RenderGatewayClass returns an upstream Gateway-API GatewayClass with
// the F5 CNE controllerName so FLO's f5-cne-controller picks up Gateway
// objects that reference this class. The historical BNKGatewayClassConfig
// CRD this template used to render does not exist in BNK 2.2.0 — see
// AGENTS.md #20.
//
// TMM replica counts are driven by CNEInstance.spec.deploymentSize +
// per-DPU scaling, not by a GatewayClass parameter.
func RenderGatewayClass(name string) (string, error) {
	if name == "" {
		name = "bnk-gatewayclass"
	}
	return renderTemplate("templates/bnk-gatewayclass.yaml.tmpl", GatewayClassInputs{
		GatewayClassName: name,
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

// CalicoInputs feeds calico-custom-resources.yaml.tmpl.
type CalicoInputs struct {
	PodCIDR string
}

// RenderCalicoCustomResources produces the calico Installation +
// APIServer CRs with the cluster's pod CIDR.
func RenderCalicoCustomResources(p *poc.PoC) (string, error) {
	in := CalicoInputs{
		PodCIDR: "10.233.64.0/18",
	}
	return renderTemplate("templates/network/calico-custom-resources.yaml.tmpl", in)
}

// NFSInputs feeds nfs-storageclass.yaml.tmpl.
type NFSInputs struct {
	NFSServer string
	NFSPath   string
}

// RenderNFSStorageClass produces the NFS StorageClass YAML.
func RenderNFSStorageClass(p *poc.PoC) (string, error) {
	server := p.Network.NFSServer
	path := p.Network.NFSPath
	if server == "" {
		server = "192.168.100.1"
	}
	if path == "" {
		path = "/srv/nfs/f5-bnk"
	}
	return renderTemplate("templates/nfs-storageclass.yaml.tmpl", NFSInputs{
		NFSServer: server,
		NFSPath:   path,
	})
}

var _ = strings.Builder{} // keep strings import for future use
