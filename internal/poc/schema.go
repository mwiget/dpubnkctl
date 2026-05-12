// Package poc defines the on-disk schema for a PoC repo (poc.yaml).
//
// poc.yaml is the source of truth: tear-down and redeploy read only this
// file. Anything not captured here is not part of the PoC.
package poc

import "time"

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
	BMC  *BMC   `yaml:"bmc,omitempty"`
	DPUs []DPU  `yaml:"dpus,omitempty"`
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
	VLANs    []DPUVLAN `yaml:"vlans,omitempty"`        // per-DPU VLAN interfaces
}

// DPUVLAN describes one OVS internal VLAN interface created on the DPU
// after flash. For LAG mode all VLANs hang off br-lag; for non-LAG the
// uplink decides which bridge (sf-external for p0, sf-internal for p1).
type DPUVLAN struct {
	Name           string `yaml:"name"`            // interface name on DPU, e.g. external0
	Tag            int    `yaml:"tag"`             // 802.1q VLAN id
	IP             string `yaml:"ip"`              // CIDR, e.g. 10.10.40.5/24
	DefaultGateway string `yaml:"default_gateway,omitempty"`
	Uplink         string `yaml:"uplink,omitempty"` // p0 | p1 — non-LAG only
}

// Provisioning holds inputs needed to render bf.conf and execute the
// flash. Most fields are PoC-wide; per-DPU VLAN/IP details live on DPU.
type Provisioning struct {
	DPUPasswordHashRef string   `yaml:"dpu_password_hash_ref"` // path under repo, gitignored — content of `openssl passwd -1 '<password>'`
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
