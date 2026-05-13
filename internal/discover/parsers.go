package discover

import (
	"bufio"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// parseOSRelease parses the contents of /etc/os-release.
func parseOSRelease(s string) OSRelease {
	var o OSRelease
	scan := bufio.NewScanner(strings.NewReader(s))
	for scan.Scan() {
		line := strings.TrimSpace(scan.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.Trim(v, `"'`)
		switch k {
		case "ID":
			o.ID = v
		case "VERSION_ID":
			o.VersionID = v
		case "PRETTY_NAME":
			o.PrettyName = v
		}
	}
	return o
}

// parseIPAddrJSON parses the JSON output of `ip -j addr`.
func parseIPAddrJSON(s string) []Interface {
	var raw []struct {
		IFName   string `json:"ifname"`
		Address  string `json:"address"`
		MTU      int    `json:"mtu"`
		AddrInfo []struct {
			Local string `json:"local"`
		} `json:"addr_info"`
	}
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return nil
	}
	out := make([]Interface, 0, len(raw))
	for _, r := range raw {
		iface := Interface{Name: r.IFName, MAC: r.Address, MTU: r.MTU}
		for _, a := range r.AddrInfo {
			if a.Local != "" {
				iface.IPs = append(iface.IPs, a.Local)
			}
		}
		out = append(out, iface)
	}
	return out
}

// lspciDPULine matches one line of `lspci -nn -d 15b3:`.
//
//	03:00.0 Ethernet controller [0200]: Mellanox Technologies MT43244 BlueField-3 ... [15b3:a2dc] (rev 01)
//
// Capture groups: 1=PCI 2=device-class-hex 3=description 4=vendor:device.
var lspciDPULine = regexp.MustCompile(`^([0-9a-f]{2}:[0-9a-f]{2}\.[0-9a-f])\s+[^[]+\[([0-9a-f]{4})\]:\s+(.+?)\s+\[([0-9a-f]{4}:[0-9a-f]{4})\]`)

// parseLspciDPUs returns one DPUDetail per Mellanox/NVIDIA function.
// BlueField devices expose multiple functions per card (PF0/PF1); we keep
// every function and let downstream merge by PCI domain:bus.
//
// looksLikeDPUOS returns true when any captured function is a PCI bridge
// (class 0604). Servers with a BF3 attached only see Ethernet controllers
// (class 0200); the SoC's own OS sees its internal PCIe bridges plus its
// Ethernet functions, so a bridge in the 15b3:* set is the give-away.
func parseLspciDPUs(s string) (dpus []DPUDetail, looksLikeDPUOS bool) {
	scan := bufio.NewScanner(strings.NewReader(s))
	for scan.Scan() {
		line := strings.TrimSpace(scan.Text())
		m := lspciDPULine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if m[2] == "0604" {
			looksLikeDPUOS = true
			// Don't add the bridge itself to dpus[] — bridges are not
			// real DPU functions.
			continue
		}
		dpus = append(dpus, DPUDetail{
			PCI:         m[1],
			Description: m[3],
			DeviceID:    m[4],
		})
	}
	return dpus, looksLikeDPUOS
}

// ipmitoolLanLine matches "Key  : Value" pairs in ipmitool lan print output.
var ipmitoolLanLine = regexp.MustCompile(`^([A-Za-z][A-Za-z 0-9]+?)\s*:\s*(.+?)\s*$`)

// parseIpmitoolLan extracts BMC IP, MAC, gateway from `ipmitool lan print`.
func parseIpmitoolLan(s string) *BMCInfo {
	bmc := &BMCInfo{Source: "ipmitool"}
	scan := bufio.NewScanner(strings.NewReader(s))
	for scan.Scan() {
		m := ipmitoolLanLine.FindStringSubmatch(scan.Text())
		if m == nil {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(m[1]))
		val := strings.TrimSpace(m[2])
		switch key {
		case "ip address":
			if val != "" && val != "0.0.0.0" {
				bmc.IP = val
			}
		case "mac address":
			bmc.MAC = val
		case "default gateway ip":
			if val != "0.0.0.0" {
				bmc.Gateway = val
			}
		}
	}
	if bmc.IP == "" {
		return nil
	}
	return bmc
}

// mlxconfigRow matches one configuration row, keeping the three value
// columns (Default, Current, Next Boot).
//
//	        INTERNAL_CPU_MODEL                          EMBEDDED_CPU(1)        EMBEDDED_CPU(1)        EMBEDDED_CPU(1)
//	*       NUM_OF_VFS                                  16                     46                     46
//
// mlxconfig prefixes rows where Current != Default with `*`. Allow an
// optional leading marker. Tabs and runs of spaces both occur in the wild.
var mlxconfigRow = regexp.MustCompile(`^\s*\*?\s+([A-Z][A-Z0-9_]+(?:\[\d+\])?)\s+(\S+)\s+(\S+)\s+(\S+)\s*$`)

// stripParen extracts "EMBEDDED_CPU" from "EMBEDDED_CPU(1)" or returns
// the input unchanged.
func stripParen(s string) string {
	if i := strings.IndexByte(s, '('); i > 0 {
		return s[:i]
	}
	return s
}

// parseMlxconfig extracts the fields dpubnkctl cares about from the
// `mlxconfig -d <pci> -e q` output. Raw keeps the full Current column.
func parseMlxconfig(s string) *DPUMlxconfig {
	cfg := &DPUMlxconfig{Raw: map[string]string{}}
	scan := bufio.NewScanner(strings.NewReader(s))
	for scan.Scan() {
		line := scan.Text()
		m := mlxconfigRow.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		key := m[1]
		current := stripParen(m[3])
		next := stripParen(m[4])
		cfg.Raw[key] = current

		if current != next {
			cfg.PendingReboot = append(cfg.PendingReboot, key)
		}

		switch key {
		case "INTERNAL_CPU_MODEL":
			cfg.InternalCPUModel = current
		case "LINK_TYPE_P1":
			cfg.LinkTypeP1 = current
		case "LINK_TYPE_P2":
			cfg.LinkTypeP2 = current
		case "LAG_RESOURCE_ALLOCATION":
			cfg.LAGResourceAllocation = current
		case "NUM_OF_VFS":
			if n, err := strconv.Atoi(current); err == nil {
				cfg.NumOfVFs = n
			}
		case "PF_TOTAL_SF":
			if n, err := strconv.Atoi(current); err == nil {
				cfg.PFTotalSF = n
			}
		}
	}
	if len(cfg.Raw) == 0 {
		return nil
	}
	return cfg
}

// parseRshimMisc parses `cat /dev/rshim0/misc` style key/value pairs.
//
//	DEV_NAME        rshim0
//	DEV_INFO        BlueField-3(Rev 1)
//	BF mode         DPU(1)
//	BOOT_MODE       1
//	UUID            ...
var rshimMiscLine = regexp.MustCompile(`^(\S+(?:\s\S+)*?)\s{2,}(.+?)\s*$`)

func parseRshimMisc(s string) map[string]string {
	out := map[string]string{}
	scan := bufio.NewScanner(strings.NewReader(s))
	for scan.Scan() {
		m := rshimMiscLine.FindStringSubmatch(scan.Text())
		if m == nil {
			continue
		}
		out[m[1]] = m[2]
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
