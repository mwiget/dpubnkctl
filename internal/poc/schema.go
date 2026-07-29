// Package poc defines the on-disk schema for a PoC repo (poc.yaml).
//
// poc.yaml is the source of truth: tear-down and redeploy read only this
// file. Anything not captured here is not part of the PoC.
package poc

import (
	"fmt"
	"net"
	"time"
)

const (
	APIVersion = "dpubnkctl.f5.com/v1alpha1"
	Kind       = "PoC"
	FileName   = "poc.yaml"
)

// BFB fetch modes for Provisioning.BFBFetch. "push" (the default when
// empty) downloads the BFB into the operator's local cache and SFTP-
// pushes it to each host; "host" makes each host curl the BFB directly
// so it never round-trips the runner. See Provisioning.BFBFetch.
const (
	BFBFetchPush = "push"
	BFBFetchHost = "host"

	// DefaultBFBHostCacheDir is where `bfb_fetch: host` stages the image
	// on the host when Provisioning.BFBHostCacheDir is unset.
	DefaultBFBHostCacheDir = "/var/cache/dpubnkctl/bfb"
)

// Join transports for Network.JoinTransport. "vlan" (the default when
// empty) is the original behavior: DPUs join k8s over a data-plane VLAN
// IP and reach the apiserver at network.cluster_apiserver_address.
// "rshim" joins over the host↔DPU tmfifo link instead — the DPU's
// kubelet --node-ip is its tmfifo IP and the apiserver is reached at the
// host's tmfifo IP. The data VLAN is left for TMM/CNE traffic set up
// later by deploy/SF-CNI. See docs/specs/dpubnkctl-rshim-join-topology.md.
const (
	JoinTransportVLAN  = "vlan"
	JoinTransportRshim = "rshim"

	// DefaultTmfifoHostIP / DefaultTmfifoDPUIP are the point-to-point /30
	// the BlueField rshim driver uses by default (host .1, DPU .2). Used
	// for single-host rshim when network.tmfifo_cidr is unset.
	DefaultTmfifoHostIP = "192.168.100.1/30"
	DefaultTmfifoDPUIP  = "192.168.100.2/30"
)

// DPU-internet modes for Provisioning.DPUInternet. "host-nat" ports the
// D-020 host-MASQUERADE path so the DPU reaches the internet (for the apt
// install during join) via the host over tmfifo. "oob" assumes the DPU
// already has internet on its oob_net0 mgmt port. "none" disables the
// setup-dpu-networking step. Empty resolves to host-nat under rshim and
// none otherwise — see PoC.EffectiveDPUInternet.
const (
	DPUInternetHostNAT = "host-nat"
	DPUInternetOOB     = "oob"
	DPUInternetNone    = "none"
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

// EffectiveDPUInternet resolves the DPU-internet mode: the explicit
// provisioning.dpu_internet if set, else host-nat under rshim and none
// otherwise. This is what the setup-dpu-networking step keys off.
func (p *PoC) EffectiveDPUInternet() string {
	if p.Provisioning.DPUInternet != "" {
		return p.Provisioning.DPUInternet
	}
	if p.Network.IsRshim() {
		return DPUInternetHostNAT
	}
	return DPUInternetNone
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
	// Ignored when JoinTransport == "rshim" (the DPU node-ip is its
	// tmfifo IP; the host keeps its ssh.address node-ip).
	NodeIPRole string `yaml:"node_ip_role,omitempty"`

	// JoinTransport selects how DPUs join the cluster: "vlan" (default
	// when empty — the original data-plane-VLAN join) or "rshim" (join
	// over the host↔DPU tmfifo link, with internet via host NAT). See
	// the JoinTransport* consts and docs/specs/dpubnkctl-rshim-join-topology.md.
	JoinTransport string `yaml:"join_transport,omitempty"`

	// TmfifoCIDR is the orchestrator-managed tmfifo address pool for
	// rshim joins across multiple hosts (e.g. "192.168.0.0/24"). When
	// set, dpubnkctl carves a unique /30 per DPU from this pool and
	// records the allocation back into each host.tmfifo_ip and
	// dpu.tmfifo_ip — replacing the per-host 192.168.100.x /30 that
	// collides when more than one host is present. Unset ⇒ single-host
	// rshim uses the rshim driver default 192.168.100.1/.2 /30.
	TmfifoCIDR string `yaml:"tmfifo_cidr,omitempty"`
}

// IsRshim reports whether DPUs join over the rshim/tmfifo link.
func (n Network) IsRshim() bool { return n.JoinTransport == JoinTransportRshim }

// HostDataPlane describes the host's data-plane VLAN sub-interfaces.
// dpubnkctl writes a netplan that adds one VLAN sub-interface per entry
// onto a host data-plane PF. VLAN sub-if names follow <Role><Tag> (e.g.
// "external40", "internal41") and align with the OVS port names on the
// DPU side so the same VLAN is identifiable end-to-end. No bond on the
// host — bonding is handled by the DPU when the DPU runs in LAG mode.
//
// ParentIface is the default PF for every VLAN. In LAG mode that single
// PF is correct: the DPU has one bridge (br-lag) and pf0hpf is its only
// host-facing port, so all host VLANs ride PF0.
//
// In NON-LAG mode the DPU has *two* bridges — sf-external (p0, pf0hpf)
// and sf-internal (p1, pf1hpf) — and the eswitch delivers everything
// arriving on host PF0 to pf0hpf regardless of VLAN tag. Stacking both
// VLANs on one host PF therefore strands the sf-internal VLAN: it
// reaches sf-external and leaves via p0, never p1. Each VLAN must hang
// off the host PF that corresponds to its DPU uplink, which is what the
// per-VLAN ParentIface override below is for. See AGENTS.md #33.
type HostDataPlane struct {
	ParentIface string              `yaml:"parent_iface"` // default PF for VLANs that don't override it, e.g. "ens16f0np0"
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

	// ParentIface overrides HostDataPlane.ParentIface for this VLAN
	// only. Empty (the common case) inherits the block default.
	//
	// Set this on non-LAG DPUs, where the host needs one PF per DPU
	// bridge: the VLAN whose DPU counterpart has `uplink: p1` must sit
	// on the host's PF1 (e.g. enp13s0f1np1), because PF0 traffic lands
	// on pf0hpf/sf-external no matter what tag it carries. Validate
	// cross-checks this against the DPU's uplinks and errors when a
	// non-LAG host multiplexes both uplinks onto one PF.
	ParentIface string `yaml:"parent_iface,omitempty"`
}

// ParentFor returns the PF that VLAN v's sub-interface hangs off: the
// per-VLAN override when set, otherwise the block-level default.
func (dp *HostDataPlane) ParentFor(v HostDataPlaneVLAN) string {
	if dp == nil {
		return v.ParentIface
	}
	if v.ParentIface != "" {
		return v.ParentIface
	}
	return dp.ParentIface
}

// ParentIfaces returns every distinct PF the host's VLANs hang off, in
// first-seen order. Callers that must touch each parent once (netplan
// `ethernets:` stanzas, the post-flash ghost-PF recovery) iterate this
// rather than assuming a single PF.
func (dp *HostDataPlane) ParentIfaces() []string {
	if dp == nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, v := range dp.VLANs {
		p := dp.ParentFor(v)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	// A data_plane block with no VLANs still names a parent worth
	// checking for ghost state.
	if len(out) == 0 && dp.ParentIface != "" {
		out = append(out, dp.ParentIface)
	}
	return out
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

	// TmfifoIP is the host-side tmfifo_net0 address (CIDR) used to reach
	// this host's DPU(s) over rshim. For single-host rshim it is the
	// rshim driver default 192.168.100.1/30; under network.tmfifo_cidr
	// it is allocated as the .1 of the DPU's carved /30 and persisted
	// here so redeploys are idempotent. Empty ⇒ 192.168.100.1/30.
	TmfifoIP string `yaml:"tmfifo_ip,omitempty"`
}

// TmfifoHostIP returns the host-side tmfifo CIDR, defaulting to the rshim
// driver's 192.168.100.1/30 when TmfifoIP is unset.
func (h *Host) TmfifoHostIP() string {
	if h != nil && h.TmfifoIP != "" {
		return h.TmfifoIP
	}
	return DefaultTmfifoHostIP
}

type SSH struct {
	Address  string `yaml:"address"`
	Port     int    `yaml:"port"`
	User     string `yaml:"user"`
	KeyRef   string `yaml:"key_ref"` // path under keys/, gitignored
	Jumphost string `yaml:"jumphost,omitempty"`

	// JumphostUser overrides SSH.User for the jumphost hop only.
	// Default (empty) reuses SSH.User. Useful when the jumphost account
	// differs from the target's account.
	JumphostUser string `yaml:"jumphost_user,omitempty"`

	// JumphostKeyRef overrides SSH.KeyRef for the jumphost hop only.
	// Default (empty) reuses SSH.KeyRef — fine when the same key opens
	// both hops. Set this when the jumphost account is authorised with
	// a *different* key than the target (e.g. operator's workstation
	// key opens the jumphost, while a separate per-lab key opens the
	// hosts behind it). Path conventions match SSH.KeyRef: relative
	// paths resolve against the PoC repo root, absolute paths land
	// outside (e.g. ~/.ssh/id_ed25519 captured at /workspace/lab/...).
	JumphostKeyRef string `yaml:"jumphost_key_ref,omitempty"`
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
	TmfifoIP string    `yaml:"tmfifo_ip,omitempty"`    // DPU-side tmfifo_net0 CIDR, e.g. 192.168.100.2/30

	// TmfifoIface is the HOST-side rshim network interface for this
	// DPU's tmfifo link — "tmfifo_net0" for the first BlueField in a
	// host, "tmfifo_net1" for the second, and so on. Empty defaults to
	// tmfifo_net0, which is correct for the overwhelmingly common
	// single-DPU host.
	//
	// It has to be per-DPU because each BlueField presents its own rshim
	// device and its own host interface. dpubnkctl previously hardcoded
	// tmfifo_net0 everywhere, so on a two-DPU host the second card's
	// link was never brought up or addressed (issue #20).
	//
	// dpubnkctl can't reliably infer the PCI→rshim-index mapping without
	// probing the host, so it is declared here. To read it off a live
	// host, match the DEV_NAME line against the DPU's PCI address:
	//
	//	for r in /dev/rshim*; do echo "$r: $(sudo cat $r/misc | grep DEV_NAME)"; done
	TmfifoIface string `yaml:"tmfifo_iface,omitempty"`
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

// DefaultTmfifoIface is the host-side rshim interface for the first (and
// usually only) BlueField in a host.
const DefaultTmfifoIface = "tmfifo_net0"

// TmfifoNetIface returns the host-side rshim interface for this DPU's
// link, defaulting to tmfifo_net0.
func (d *DPU) TmfifoNetIface() string {
	if d != nil && d.TmfifoIface != "" {
		return d.TmfifoIface
	}
	return DefaultTmfifoIface
}

// TmfifoHostIP returns the HOST-side address of this DPU's point-to-point
// tmfifo link, as a CIDR.
//
// Derived from the DPU's own /30 rather than stored: in a /30 the first
// usable address is the host and the second is the DPU, so the host side
// is always (network + 1). That derivation is what makes multi-DPU hosts
// work — each link gets its own /30, and the host end of each follows
// from the DPU end without a second field to keep in sync.
//
// Falls back to the rshim driver default when tmfifo_ip is unset or
// unparseable; validate reports the malformed value separately.
func (d *DPU) TmfifoHostIP() string {
	if d == nil || d.TmfifoIP == "" {
		return DefaultTmfifoHostIP
	}
	_, ipnet, err := net.ParseCIDR(d.TmfifoIP)
	if err != nil {
		return DefaultTmfifoHostIP
	}
	ones, bits := ipnet.Mask.Size()
	if bits != 32 || ones != 30 {
		return DefaultTmfifoHostIP
	}
	base := ipnet.IP.To4()
	if base == nil {
		return DefaultTmfifoHostIP
	}
	host := net.IPv4(base[0], base[1], base[2], base[3]+1)
	return fmt.Sprintf("%s/30", host.String())
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

	// BFBOnHost, when non-empty, is an absolute path on the data-plane
	// host where the BFB image is already staged. Setting this skips
	// both the local download (EnsureBFB) and the SFTP upload
	// (pushAndFlash), reusing the pre-staged file in place — designed
	// for operators on slow/expensive links (transatlantic VPN, metered
	// home internet, air-gapped labs) who can stage the BFB into the lab
	// once via curl/wget from a fast pipe.
	//
	// When set:
	//   - provision_dpu skips EnsureBFB entirely (no local cache use,
	//     no network egress from the laptop for the BFB)
	//   - pushAndFlash skips PushFile and uses this path as remoteBFB
	//     directly
	//   - a remote stat confirms the file exists and has size > 0 before
	//     bfb-install runs; missing file fails fast with a clear error
	//
	// Path must be absolute. BFBURL and BFBCacheDir are ignored when
	// this is set (validate emits a warning). Typical usage:
	//
	//   provisioning:
	//     bfb_on_host: /var/cache/dpubnkctl/bfb/bf-bundle-3.2.0-113_25.10_ubuntu-24.04_64k_prod.bfb
	//
	// with the operator having pre-staged the file via:
	//
	//   ssh host 'wget -O <path> https://content.mellanox.com/.../<image>.bfb'
	BFBOnHost string `yaml:"bfb_on_host,omitempty"`

	// BFBSHA256, when non-empty, is the expected sha256 hex digest of the
	// BFB image and overrides the binary-pinned version.BFBImageSHA256.
	// Use it to pin a custom or older BFB whose digest differs from the
	// release-pinned one. Precedence: this value > version.BFBImageSHA256
	// > empty (integrity not enforced — validate warns). Applies to every
	// fetch mode: the local download (push), the pre-staged file
	// (bfb_on_host), and the host-fetched file (bfb_fetch: host).
	BFBSHA256 string `yaml:"bfb_sha256,omitempty"`

	// BFBFetch selects how the BFB reaches the host: "push" (default when
	// empty) downloads to the operator's local cache and SFTP-pushes it;
	// "host" makes the host curl the BFB directly from BFBURL/BFBBaseURL
	// (see BFBFetchPush/BFBFetchHost), so a 1.5 GB image never round-trips
	// a slow runner→host link. `bfb_fetch: host` and an explicit
	// bfb_on_host are mutually exclusive (validate errors). The
	// `--bfb-fetch` flag on `provision dpu` overrides this.
	BFBFetch string `yaml:"bfb_fetch,omitempty"`

	// BFBHostCacheDir is the directory on the host where `bfb_fetch: host`
	// stages the image (as <dir>/<bfb_image>). Defaults to
	// DefaultBFBHostCacheDir (/var/cache/dpubnkctl/bfb) when empty. A
	// staged file whose sha256 already matches is reused without re-
	// fetching, so the dir doubles as a persistent per-host BFB cache.
	BFBHostCacheDir string `yaml:"bfb_host_cache_dir,omitempty"`

	// DPUInternet selects how the DPU gets internet before the join-time
	// apt install. "host-nat" (the D-020 path) MASQUERADEs DPU traffic
	// through the host over tmfifo; "oob" assumes the DPU already has
	// internet on oob_net0; "none" skips the setup-dpu-networking step.
	// Empty resolves via PoC.EffectiveDPUInternet (host-nat under rshim,
	// none otherwise). See the DPUInternet* consts.
	DPUInternet string `yaml:"dpu_internet,omitempty"`
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
