package poc

import (
	"fmt"
	"os"
	"path/filepath"
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
	var p PoC
	if err := yaml.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &p, nil
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
