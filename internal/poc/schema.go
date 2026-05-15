// Package poc defines the on-disk schema for a PoC repo (poc.yaml).
//
// poc.yaml is the source of truth: tear-down and redeploy read only this
// file. Anything not captured here is not part of the PoC.
package poc

import (
	"fmt"
	"time"
)

const (
	APIVersion = "dpubnkctl.f5.com/v1alpha1"
	Kind       = "PoC"
	FileName   = "poc.yaml"
)

type PoC struct {
	APIVersion   string       `yaml:"apiVersion"`
	Kind         string       `yaml:"kind"`
	Metadata     Metadata     `yaml:"metadata"`
	Versions     Versions     `yaml:"versions"`
	Topology     Topology     `yaml:"topology"`
	Network      Network      `yaml:"network"`
	Provisioning Provisioning `yaml:"provisioning"`
	Hosts        []Host       `yaml:"hosts"`
	BNK          BNK          `yaml:"bnk"`
	Status       Status       `yaml:"status"`
	Agent        Agent        `yaml:"agent"`
	BNKForge     BNKForge     `yaml:"bnk_forge,omitempty"`
}

type Metadata struct {
	Name             string    `yaml:"name"`
	Customer         string    `yaml:"customer"`
	Created          time.Time `yaml:"created"`
	DpubnkctlVersion string    `yaml:"dpubnkctl_version"`
	BNKVersion       string    `yaml:"bnk_version"`
}

// Versions are pinned defaults from the binary. Override only with cause.
type Versions struct {
	DOCA       string `yaml:"doca"`
	BFBImage   string `yaml:"bfb_image"`
	BFBURL     string `yaml:"bfb_url,omitempty"`
	FLOChart   string `yaml:"flo_chart"`
	K8s        string `yaml:"k8s"`
	Containerd string `yaml:"containerd"`
	Runc       string `yaml:"runc"`
	PauseTag   string `yaml:"pause_tag"`
}

// Topology is set by the pre-sales SE before discovery confirms it.
type Topology struct {
	Mode                string `yaml:"mode"` // single-node | multi-node | ""
	LAG                 bool   `yaml:"lag"`
	ControlPlaneCount   int    `yaml:"control_plane_count"`
	ExpectedHosts       int    `yaml:"expected_hosts"`
	ExpectedDPUsPerHost int    `yaml:"expected_dpus_per_host"`
}

type Network struct {
	InternalCIDR string `yaml:"internal_cidr"`
	PodMTU       int    `yaml:"pod_mtu"`
	DPUMTU       int    `yaml:"dpu_mtu"`
	VLANs        []VLAN `yaml:"vlans"`
	NFSServer    string `yaml:"nfs_server,omitempty"`
	NFSPath      string `yaml:"nfs_path,omitempty"`

	// ClusterAPIServerAddress is the address apiserver advertises and
	// every node uses in its kubeconfig. Set to a routable IP/VIP on
	// the data-plane network (e.g. "10.10.41.66"). When set, kubespray
	// is configured with loadbalancer_apiserver_localhost=false and
	// loadbalancer_apiserver pointing here — eliminates the
	// 127.0.0.1 nginx-proxy hack and lets externally-joined DPUs talk
	// to the apiserver without extra plumbing.
	ClusterAPIServerAddress string `yaml:"cluster_apiserver_address,omitempty"`

	// NodeIPRole names the VLAN role that provides each node's
	// kubelet --node-ip. Hosts pick host.data_plane.vlans[role==X];
	// DPUs pick dpu.vlans[role==X]. When unset, hosts fall back to
	// ssh.address and DPUs let kubelet auto-detect. Typically set to
	// the same role used for ClusterAPIServerAddress (e.g. "internal").
	NodeIPRole string `yaml:"node_ip_role,omitempty"`
}

// HostDataPlane describes the host's data-plane VLAN sub-interfaces.
// dpubnkctl writes a netplan that adds one VLAN sub-interface per entry
// onto the host's single data-plane PF (ParentIface). VLAN sub-if names
// follow <Role><Tag> (e.g. "external40", "internal41") and align with
// the OVS port names on the DPU side so the same VLAN is identifiable
// end-to-end. No bond on the host — bonding is handled by the DPU when
// the DPU runs in LAG mode.
type HostDataPlane struct {
	ParentIface string              `yaml:"parent_iface"` // e.g. "ens16f0np0"
	MTU         int                 `yaml:"mtu,omitempty"` // default for sub-ifs; falls back to network.dpu_mtu
	VLANs       []HostDataPlaneVLAN `yaml:"vlans"`         // one entry per VLAN this host needs an IP on
}

// HostDataPlaneVLAN is one VLAN sub-interface on the host.
// Sub-interface name is derived as Role+Tag (e.g. "internal41",
// "storage50"). Role is a short lowercase identifier; well-known values
// include external, internal, storage, mgmt, replication — but any
// matching `^[a-z][a-z0-9]{0,9}$` is accepted as long as Role+Tag fits
// the Linux 15-char interface-name limit.
type HostDataPlaneVLAN struct {
	Role string `yaml:"role"` // e.g. external, internal, storage
	Tag  int    `yaml:"tag"`  // 802.1q VLAN id
	IP   string `yaml:"ip"`   // CIDR, e.g. "10.10.41.66/24"
	MTU  int    `yaml:"mtu,omitempty"`
}

// PortName returns the netplan VLAN sub-interface name (e.g. "internal41").
func (v HostDataPlaneVLAN) PortName() string {
	return fmt.Sprintf("%s%d", v.Role, v.Tag)
}

// VLANByRole returns the host's data-plane VLAN with the given role,
// or nil if no data_plane block is present or no VLAN matches.
func (h *Host) VLANByRole(role string) *HostDataPlaneVLAN {
	if h == nil || h.DataPlane == nil || role == "" {
		return nil
	}
	for i := range h.DataPlane.VLANs {
		if h.DataPlane.VLANs[i].Role == role {
			return &h.DataPlane.VLANs[i]
		}
	}
	return nil
}

// VLANByRole returns the DPU's VLAN with the given role, or nil if none.
func (d *DPU) VLANByRole(role string) *DPUVLAN {
	if d == nil || role == "" {
		return nil
	}
	for i := range d.VLANs {
		if d.VLANs[i].Role == role {
			return &d.VLANs[i]
		}
	}
	return nil
}

type VLAN struct {
	Name   string `yaml:"name"`
	ID     int    `yaml:"id"`
	Subnet string `yaml:"subnet"`
}

type Host struct {
	Name string `yaml:"name"`
	Role string `yaml:"role"` // control-plane | worker | both
	SSH  SSH    `yaml:"ssh"`
	// MgmtIface is the kernel interface name on the host whose IPv4
	// address equals SSH.Address (e.g. "eth0" or "eno1"). Populated by
	// `dpubnkctl discover` by matching the SSH address against the
	// probed `ip -j addr show` output. Used by the diagram to label the
	// mgmt-plane interface symmetrically with the DPU's "oob_net0".
	// Optional — older PoCs without it render with just the IP.
	MgmtIface string         `yaml:"mgmt_iface,omitempty"`
	BMC       *BMC           `yaml:"bmc,omitempty"`
	DataPlane *HostDataPlane `yaml:"data_plane,omitempty"`
	DPUs      []DPU          `yaml:"dpus,omitempty"`
}

type SSH struct {
	Address  string `yaml:"address"`
	Port     int    `yaml:"port"`
	User     string `yaml:"user"`
	KeyRef   string `yaml:"key_ref"` // path under keys/, gitignored
	Jumphost string `yaml:"jumphost,omitempty"`
}

type BMC struct {
	Address     string `yaml:"address"`
	User        string `yaml:"user"`
	PasswordRef string `yaml:"password_ref"`
	Protocol    string `yaml:"protocol"` // redfish | ipmi
}

type DPU struct {
	Serial   string    `yaml:"serial"`
	PCI      string    `yaml:"pci"`
	BMCIP    string    `yaml:"bmc_ip,omitempty"`
	Mode     string    `yaml:"mode"` // dpu | nic
	LAG      bool      `yaml:"lag"`
	Hostname string    `yaml:"hostname,omitempty"`     // DPU OS hostname (set before flash)
	TmfifoIP string    `yaml:"tmfifo_ip,omitempty"`    // tmfifo_net0 CIDR, e.g. 192.168.100.2/30
	// OOBIP is the DPU's oob_net0 (GigE OOB mgmt port) address as CIDR
	// (e.g. "192.168.68.96/22"), DHCP-learned at first boot and captured
	// after flash. Stored as full CIDR — matching tmfifo_ip's shape and
	// preserving the netmask DHCP supplied, which is useful for routing
	// diagnostics and mgmt-subnet recap in reports. Diagram + status
	// strip the prefix for display.
	OOBIP    string    `yaml:"oob_ip,omitempty"`
	VLANs    []DPUVLAN `yaml:"vlans,omitempty"`        // per-DPU VLAN interfaces

}

// DPUVLAN describes one OVS internal VLAN interface created on the DPU
// after flash. For LAG mode all VLANs hang off br-lag; for non-LAG the
// uplink decides which bridge (sf-external for p0, sf-internal for p1).
//
// The OVS port name is derived as Role+Tag (e.g. "external40",
// "internal41", "storage50") so the same VLAN is identifiable on both
// DPU and host. Role is any short lowercase identifier; common values
// are external, internal, storage, mgmt, replication.
type DPUVLAN struct {
	Role           string `yaml:"role"`           // e.g. external, internal, storage
	Tag            int    `yaml:"tag"`            // 802.1q VLAN id
	IP             string `yaml:"ip"`             // CIDR, e.g. 10.10.40.5/24
	DefaultGateway string `yaml:"default_gateway,omitempty"`
	Uplink         string `yaml:"uplink,omitempty"` // p0 | p1 — non-LAG only
}

// PortName returns the OVS port name (e.g. "external40").
func (v DPUVLAN) PortName() string {
	return fmt.Sprintf("%s%d", v.Role, v.Tag)
}

// Provisioning holds inputs needed to render bf.conf and execute the
// flash. Most fields are PoC-wide; per-DPU VLAN/IP details live on DPU.
type Provisioning struct {
	DPUPasswordHashRef string   `yaml:"dpu_password_hash_ref"` // path under repo, gitignored — content of `openssl passwd -1 '<password>'`
	OperatorPubkeyRef  string   `yaml:"operator_pubkey_ref,omitempty"` // operator's SSH pubkey, written to /home/ubuntu/.ssh/authorized_keys on the DPU at flash time. If empty, derived from the per-host SSH key + ".pub".
	DPUDNS             []string `yaml:"dpu_dns"`
	DPUDNSDomains      []string `yaml:"dpu_dns_domains,omitempty"`
	DPUNTP             []string `yaml:"dpu_ntp"`
	BFBURL             string   `yaml:"bfb_url,omitempty"`     // override of binary-pinned default
	BFBCacheDir        string   `yaml:"bfb_cache_dir"`          // ~/.cache/dpubnkctl/bfb by default
}

type BNK struct {
	FARKeyRef      string `yaml:"far_key_ref"`
	JWTRef         string `yaml:"jwt_ref"`
	ExternalSelfIP string `yaml:"external_selfip,omitempty"`
	InternalSelfIP string `yaml:"internal_selfip,omitempty"`
}

// Status records phase progression. destroy/redeploy reads this to know
// what to undo.
type Status struct {
	Discover    string    `yaml:"discover"`
	Provision   string    `yaml:"provision"`
	Cluster     string    `yaml:"cluster"`
	Deploy      string    `yaml:"deploy"`
	LastPhaseAt time.Time `yaml:"last_phase_at,omitempty"`
}

type Agent struct {
	LLMEndpoint string `yaml:"llm_endpoint,omitempty"`
	Default     string `yaml:"default"` // claude | gemini | aider | openai
}

// BNKForge controls the optional `dpubnkctl bnk-forge launch` step that
// installs (if needed) + starts the local bnk-forge stack and auto-
// creates a project matching this PoC. The bnk-forge repository
// (https://github.com/sp-prod-field/bnk-forge — currently private) must
// be cloned locally; RepoPath points to it.
type BNKForge struct {
	Enabled       bool   `yaml:"enabled"`                // master switch
	RepoPath      string `yaml:"repo_path,omitempty"`    // local clone, e.g. ~/git/bnk-forge
	AutoLaunch    bool   `yaml:"auto_launch,omitempty"`  // chain from e2e after cluster up
	URL           string `yaml:"url,omitempty"`          // default https://localhost
	AdminUsername string `yaml:"admin_username,omitempty"` // default admin
	AdminPassword string `yaml:"admin_password,omitempty"` // default changeme (dev-only)
	ProjectColor  string `yaml:"project_color,omitempty"`  // default #0a3a5c
	ProjectIcon   string `yaml:"project_icon,omitempty"`
}
