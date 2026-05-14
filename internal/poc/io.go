package poc

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/mwiget/dpubnkctl/internal/version"
)

// New returns a PoC populated with binary-pinned defaults. Caller fills in
// Metadata.Name and any topology/network/host details before writing.
func New(name string) *PoC {
	return &PoC{
		APIVersion: APIVersion,
		Kind:       Kind,
		Metadata: Metadata{
			Name:             name,
			Created:          time.Now().UTC(),
			DpubnkctlVersion: version.Version,
			BNKVersion:       version.BNKVersion,
		},
		Versions: Versions{
			DOCA:       version.DOCAVersion,
			BFBImage:   version.BFBImage,
			FLOChart:   version.FLOChartVer,
			K8s:        version.K8sVersion,
			Containerd: version.ContainerdVer,
			Runc:       version.RuncVersion,
			PauseTag:   version.PauseImageTag,
		},
		Topology: Topology{
			ControlPlaneCount: 1,
		},
		Network: Network{
			InternalCIDR: "198.18.100.0/24",
			PodMTU:       version.DefaultPodMTU,
			DPUMTU:       version.DefaultDPUMTU,
			VLANs:        []VLAN{},
		},
		Provisioning: Provisioning{
			DPUPasswordHashRef: "keys/dpu_password.hash",
			DPUDNS:             []string{"8.8.8.8", "1.1.1.1"},
			DPUNTP:             []string{"pool.ntp.org"},
			BFBCacheDir:        "~/.cache/dpubnkctl/bfb",
		},
		Hosts: []Host{},
		BNK: BNK{
			FARKeyRef: "keys/f5-far-auth-key.tgz",
			JWTRef:    "keys/.jwt",
		},
		Status: Status{
			Discover:  "pending",
			Provision: "pending",
			Cluster:   "pending",
			Deploy:    "pending",
		},
		Agent: Agent{
			Default: "claude",
		},
	}
}

func Load(dir string) (*PoC, error) {
	path := filepath.Join(dir, FileName)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	// Strict decode: yaml.v3's default silently drops unknown fields, so
	// a typo like `role:` instead of `name:` under network.vlans[] used
	// to load clean but with an empty struct — only surfacing two phases
	// later as a baffling failure. KnownFields(true) hard-fails any key
	// the schema doesn't declare; the helper below sniffs the common
	// "they used the DPU VLAN shape instead of the network.vlans shape"
	// mistake and points at the right schema.
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var p PoC
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("parse %s: %w%s", path, err, schemaHintFor(err))
	}
	return &p, nil
}

// schemaHintFor recognises common shape-confusion errors and appends a
// pointer at the right fields. yaml.v3's KnownFields error text gives us
// the offending key name but not a corrective suggestion.
func schemaHintFor(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "field role not found in type poc.VLAN") ||
		strings.Contains(msg, "field tag not found in type poc.VLAN"):
		return "\n\nhint: network.vlans[] uses { name, id, subnet } — not { role, tag, subnet }. The role/tag shape is for the per-host data_plane.vlans[] and per-DPU dpus[].vlans[] (where the role+tag combine into a Linux interface name)."
	}
	return ""
}

func (p *PoC) Save(dir string) error {
	path := filepath.Join(dir, FileName)
	data, err := yaml.Marshal(p)
	if err != nil {
		return fmt.Errorf("marshal poc: %w", err)
	}
	header := []byte("# dpubnkctl PoC declarative state — source of truth for this PoC.\n" +
		"# All inputs needed to teardown and redeploy live here.\n" +
		"# Do not edit by hand without consulting the pre-sales SE persona.\n\n")
	return os.WriteFile(path, append(header, data...), 0o644)
}
