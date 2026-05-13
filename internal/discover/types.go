// Package discover runs read-only probes against a host over SSH and
// produces a typed inventory of host + DPU state.
package discover

import "time"

// Result is the full output of DiscoverHost. Persist as JSON under
// inventory/<hostname>/discover.json.
type Result struct {
	Address      string      `json:"address"`
	DiscoveredAt time.Time   `json:"discovered_at"`
	Host         HostInfo    `json:"host"`
	BMC          *BMCInfo    `json:"bmc,omitempty"`
	DPUs         []DPUDetail `json:"dpus"`
	// IsDPU is true when this address answered SSH but the lspci probe
	// indicates we landed on the BlueField SoC's own OS instead of a
	// server hosting the DPU. The give-away: PCI bridges (class 0604)
	// appear among the 15b3:* devices on the DPU OS, but never on a
	// server (where the BF3 SoC presents as Ethernet controllers, class
	// 0200, only). Range scans set this to exclude DPU IPs from the
	// host-candidate list since DPUs belong to a server's `dpus[]` block,
	// not as top-level hosts in `poc.yaml`.
	IsDPU    bool     `json:"is_dpu,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
	Errors   []string `json:"errors,omitempty"`
}

type HostInfo struct {
	Hostname   string      `json:"hostname"`
	Kernel     string      `json:"kernel"`
	OS         OSRelease   `json:"os"`
	Model      string      `json:"model,omitempty"`
	Interfaces []Interface `json:"interfaces"`
	Tools      Tools       `json:"tools"`
	Rshim      RshimState  `json:"rshim"`
}

type OSRelease struct {
	ID         string `json:"id,omitempty"`
	VersionID  string `json:"version_id,omitempty"`
	PrettyName string `json:"pretty_name,omitempty"`
}

type Interface struct {
	Name string   `json:"name"`
	MAC  string   `json:"mac,omitempty"`
	IPs  []string `json:"ips,omitempty"`
	MTU  int      `json:"mtu,omitempty"`
}

// Tools records absolute paths to discovery-relevant tools, or "" if absent.
type Tools struct {
	Mlxconfig  string `json:"mlxconfig,omitempty"`
	BFBInstall string `json:"bfb_install,omitempty"`
	Ipmitool   string `json:"ipmitool,omitempty"`
	Rshim      string `json:"rshim,omitempty"`
	Mst        string `json:"mst,omitempty"`
}

type RshimState struct {
	Loaded  bool     `json:"loaded"`
	Devices []string `json:"devices,omitempty"` // e.g., ["/dev/rshim0"]
}

type BMCInfo struct {
	Source  string `json:"source"` // "ipmitool" | "manual"
	IP      string `json:"ip"`
	MAC     string `json:"mac,omitempty"`
	Gateway string `json:"gateway,omitempty"`
}

type DPUDetail struct {
	PCI         string         `json:"pci"`
	DeviceID    string         `json:"device_id"`         // 15b3:a2dc
	Description string         `json:"description,omitempty"`
	Mlxconfig   *DPUMlxconfig  `json:"mlxconfig,omitempty"`
	RshimMisc   map[string]string `json:"rshim_misc,omitempty"`
}

// DPUMlxconfig captures the BlueField NV-config settings dpubnkctl cares
// about. Raw holds every key for transparency / future use.
type DPUMlxconfig struct {
	InternalCPUModel      string            `json:"internal_cpu_model,omitempty"`
	LinkTypeP1            string            `json:"link_type_p1,omitempty"`
	LinkTypeP2            string            `json:"link_type_p2,omitempty"`
	LAGResourceAllocation string            `json:"lag_resource_allocation,omitempty"`
	NumOfVFs              int               `json:"num_of_vfs,omitempty"`
	PFTotalSF             int               `json:"pf_total_sf,omitempty"`
	PendingReboot         []string          `json:"pending_reboot,omitempty"` // keys where Next Boot != Current
	Raw                   map[string]string `json:"raw,omitempty"`
}
