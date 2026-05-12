// Package provision builds bf.conf renders, runs pre-flash readiness
// checks, and (later) drives the bfb-install execution.
package provision

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/mwiget/dpubnkctl/internal/embedded"
	"github.com/mwiget/dpubnkctl/internal/poc"
)

// RenderInputs is the flat view passed to the bf.conf templates. Built
// from PoC + Host + DPU by Render().
type RenderInputs struct {
	PasswordHash string         // hash from openssl passwd -1 '<pw>'
	DPUMtu       int            // network.dpu_mtu
	Hostname     string         // dpu.hostname (DPU OS hostname)
	TmfifoIP     string         // dpu.tmfifo_ip CIDR (e.g. 192.168.100.2/30)
	DNSServers   string         // space-separated for resolved.conf
	DNSDomains   string
	NTPServers   string
	VLANs        []RenderedVLAN
}

type RenderedVLAN struct {
	Name           string
	Tag            int
	IP             string
	DefaultGateway string
	Uplink         string // p0|p1, used by non-LAG template
}

// Render produces the bf.conf for one DPU. It picks lag vs nolag from
// dpu.LAG, then walks every required input and surfaces missing fields
// as a single descriptive error so the operator can fix them in poc.yaml.
func Render(p *poc.PoC, h *poc.Host, d *poc.DPU, repoDir string) (string, error) {
	inputs, err := buildInputs(p, h, d, repoDir)
	if err != nil {
		return "", err
	}
	tmplPath := "templates/bf-nolag.conf.tmpl"
	if d.LAG {
		tmplPath = "templates/bf-lag.conf.tmpl"
	}
	raw, err := embedded.Templates.ReadFile(tmplPath)
	if err != nil {
		return "", fmt.Errorf("load template %s: %w", tmplPath, err)
	}
	tmpl, err := template.New(filepath.Base(tmplPath)).Parse(string(raw))
	if err != nil {
		return "", fmt.Errorf("parse template %s: %w", tmplPath, err)
	}
	var out bytes.Buffer
	if err := tmpl.Execute(&out, inputs); err != nil {
		return "", fmt.Errorf("execute template %s: %w", tmplPath, err)
	}
	return out.String(), nil
}

func buildInputs(p *poc.PoC, h *poc.Host, d *poc.DPU, repoDir string) (RenderInputs, error) {
	var missing []string

	if p.Provisioning.DPUPasswordHashRef == "" {
		missing = append(missing, "provisioning.dpu_password_hash_ref")
	}
	if d.Hostname == "" {
		missing = append(missing, fmt.Sprintf("hosts[%s].dpus[%s].hostname", h.Name, d.PCI))
	}
	if d.TmfifoIP == "" {
		missing = append(missing, fmt.Sprintf("hosts[%s].dpus[%s].tmfifo_ip", h.Name, d.PCI))
	}
	if p.Network.DPUMTU == 0 {
		missing = append(missing, "network.dpu_mtu")
	}
	if len(p.Provisioning.DPUDNS) == 0 {
		missing = append(missing, "provisioning.dpu_dns")
	}
	if len(p.Provisioning.DPUNTP) == 0 {
		missing = append(missing, "provisioning.dpu_ntp")
	}
	if !d.LAG {
		for i, v := range d.VLANs {
			if v.Uplink != "p0" && v.Uplink != "p1" {
				missing = append(missing, fmt.Sprintf("hosts[%s].dpus[%s].vlans[%d].uplink (must be p0 or p1 in non-LAG mode)", h.Name, d.PCI, i))
			}
		}
	}
	for i, v := range d.VLANs {
		if v.Name == "" {
			missing = append(missing, fmt.Sprintf("hosts[%s].dpus[%s].vlans[%d].name", h.Name, d.PCI, i))
		}
		if v.IP == "" {
			missing = append(missing, fmt.Sprintf("hosts[%s].dpus[%s].vlans[%d].ip", h.Name, d.PCI, i))
		}
		if _, _, err := net.ParseCIDR(v.IP); v.IP != "" && err != nil {
			missing = append(missing, fmt.Sprintf("hosts[%s].dpus[%s].vlans[%d].ip is not a valid CIDR (%q)", h.Name, d.PCI, i, v.IP))
		}
	}

	if len(missing) > 0 {
		return RenderInputs{}, fmt.Errorf("cannot render bf.conf — missing/invalid fields:\n  - %s",
			strings.Join(missing, "\n  - "))
	}

	hash, err := readPasswordHash(repoDir, p.Provisioning.DPUPasswordHashRef)
	if err != nil {
		return RenderInputs{}, err
	}

	vlans := make([]RenderedVLAN, len(d.VLANs))
	for i, v := range d.VLANs {
		vlans[i] = RenderedVLAN{
			Name:           v.Name,
			Tag:            v.Tag,
			IP:             v.IP,
			DefaultGateway: v.DefaultGateway,
			Uplink:         v.Uplink,
		}
	}

	return RenderInputs{
		PasswordHash: hash,
		DPUMtu:       p.Network.DPUMTU,
		Hostname:     d.Hostname,
		TmfifoIP:     d.TmfifoIP,
		DNSServers:   strings.Join(p.Provisioning.DPUDNS, " "),
		DNSDomains:   strings.Join(p.Provisioning.DPUDNSDomains, " "),
		NTPServers:   strings.Join(p.Provisioning.DPUNTP, " "),
		VLANs:        vlans,
	}, nil
}

// readPasswordHash loads the openssl-passwd-1 hash from the configured
// path. Refuses to render if the hash file is missing or doesn't look
// like a $1$ MD5 crypt hash — sending the wrong format silently produces
// an unloggable DPU.
func readPasswordHash(repoDir, ref string) (string, error) {
	path := ref
	if !filepath.IsAbs(path) {
		path = filepath.Join(repoDir, ref)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("password hash file not found at %s — generate with: openssl passwd -1 '<password>' > %s", path, path)
		}
		return "", fmt.Errorf("read password hash %s: %w", path, err)
	}
	hash := strings.TrimSpace(string(data))
	if !strings.HasPrefix(hash, "$1$") && !strings.HasPrefix(hash, "$6$") {
		return "", fmt.Errorf("password hash at %s does not look like a crypt hash (must start with $1$ or $6$); regenerate with `openssl passwd -1 '<password>'`", path)
	}
	return hash, nil
}
