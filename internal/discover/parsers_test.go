package discover

import (
	"reflect"
	"testing"
)

func TestParseOSRelease(t *testing.T) {
	in := `NAME="Ubuntu"
VERSION="22.04 LTS (Jammy Jellyfish)"
ID=ubuntu
VERSION_ID="22.04"
PRETTY_NAME="Ubuntu 22.04 LTS"
`
	got := parseOSRelease(in)
	want := OSRelease{ID: "ubuntu", VersionID: "22.04", PrettyName: "Ubuntu 22.04 LTS"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseOSRelease = %+v, want %+v", got, want)
	}
}

func TestParseLspciDPUs(t *testing.T) {
	in := `03:00.0 Ethernet controller [0200]: Mellanox Technologies MT43244 BlueField-3 integrated ConnectX-7 network controller [15b3:a2dc] (rev 01)
03:00.1 Ethernet controller [0200]: Mellanox Technologies MT43244 BlueField-3 integrated ConnectX-7 network controller [15b3:a2dc] (rev 01)
17:00.0 Ethernet controller [0200]: Mellanox Technologies MT43244 BlueField-3 integrated ConnectX-7 network controller [15b3:a2dc] (rev 01)
unrelated noise
`
	got := parseLspciDPUs(in)
	if len(got) != 3 {
		t.Fatalf("got %d entries, want 3 raw functions", len(got))
	}
	if got[0].PCI != "03:00.0" || got[0].DeviceID != "15b3:a2dc" {
		t.Errorf("first entry wrong: %+v", got[0])
	}

	merged := mergeFunctionsByCard(got)
	if len(merged) != 2 {
		t.Errorf("merged: got %d cards, want 2 (03:00 and 17:00)", len(merged))
	}
}

func TestParseIpmitoolLan(t *testing.T) {
	in := `Set in Progress         : Set Complete
Auth Type Support       : NONE MD2 MD5 PASSWORD
IP Address Source       : DHCP Address
IP Address              : 192.168.1.110
Subnet Mask             : 255.255.255.0
MAC Address             : 00:25:90:aa:bb:cc
Default Gateway IP      : 192.168.1.1
`
	got := parseIpmitoolLan(in)
	if got == nil {
		t.Fatal("expected non-nil BMC info")
	}
	if got.IP != "192.168.1.110" || got.MAC != "00:25:90:aa:bb:cc" || got.Gateway != "192.168.1.1" {
		t.Errorf("BMC = %+v", got)
	}
	if got.Source != "ipmitool" {
		t.Errorf("Source = %q, want ipmitool", got.Source)
	}
}

func TestParseIpmitoolLan_NoIP(t *testing.T) {
	in := `IP Address              : 0.0.0.0
MAC Address             : 00:00:00:00:00:00
`
	if got := parseIpmitoolLan(in); got != nil {
		t.Errorf("expected nil for 0.0.0.0 IP, got %+v", got)
	}
}

func TestParseMlxconfig(t *testing.T) {
	// Trimmed real-world mlxconfig output. Header, separator, configurations.
	in := `Device #1:
----------

Device type:        BlueField3
Device:             03:00.0

Configurations:                                    Default                Current                Next Boot
        MEMIC_BAR_SIZE                              0                      0                      0
        INTERNAL_CPU_MODEL                          EMBEDDED_CPU(1)        EMBEDDED_CPU(1)        EMBEDDED_CPU(1)
        LINK_TYPE_P1                                ETH(2)                 ETH(2)                 ETH(2)
        LINK_TYPE_P2                                ETH(2)                 ETH(2)                 ETH(2)
        LAG_RESOURCE_ALLOCATION                     DISABLE(0)             ENABLE(1)              ENABLE(1)
        NUM_OF_VFS                                  16                     46                     46
        PF_TOTAL_SF                                 0                      20                     20
`
	got := parseMlxconfig(in)
	if got == nil {
		t.Fatal("expected non-nil mlxconfig")
	}
	if got.InternalCPUModel != "EMBEDDED_CPU" {
		t.Errorf("InternalCPUModel = %q", got.InternalCPUModel)
	}
	if got.LinkTypeP1 != "ETH" || got.LinkTypeP2 != "ETH" {
		t.Errorf("link types = %q/%q", got.LinkTypeP1, got.LinkTypeP2)
	}
	if got.LAGResourceAllocation != "ENABLE" {
		t.Errorf("LAG = %q", got.LAGResourceAllocation)
	}
	if got.NumOfVFs != 46 {
		t.Errorf("NumOfVFs = %d", got.NumOfVFs)
	}
	if got.PFTotalSF != 20 {
		t.Errorf("PFTotalSF = %d", got.PFTotalSF)
	}
	if len(got.PendingReboot) != 0 {
		t.Errorf("PendingReboot = %v, want empty (Current == Next Boot)", got.PendingReboot)
	}
}

func TestParseMlxconfig_PendingReboot(t *testing.T) {
	// Current differs from Next Boot for LAG_RESOURCE_ALLOCATION.
	in := `Configurations:                                    Default                Current                Next Boot
        LAG_RESOURCE_ALLOCATION                     DISABLE(0)             DISABLE(0)             ENABLE(1)
`
	got := parseMlxconfig(in)
	if got == nil {
		t.Fatal("nil")
	}
	if got.LAGResourceAllocation != "DISABLE" {
		t.Errorf("Current LAG = %q, want DISABLE", got.LAGResourceAllocation)
	}
	if len(got.PendingReboot) != 1 || got.PendingReboot[0] != "LAG_RESOURCE_ALLOCATION" {
		t.Errorf("PendingReboot = %v", got.PendingReboot)
	}
}

func TestParseMlxconfig_AsteriskMarkedRow(t *testing.T) {
	// mlxconfig prefixes non-default rows with `*` — must not break the regex.
	in := `Configurations:                                    Default                Current                Next Boot
*       NUM_OF_VFS                                  16                     46                     46
        PF_TOTAL_SF                                 0                      0                      0
`
	got := parseMlxconfig(in)
	if got == nil {
		t.Fatal("nil")
	}
	if got.NumOfVFs != 46 {
		t.Errorf("NumOfVFs = %d, want 46 (asterisk-marked row)", got.NumOfVFs)
	}
	if got.PFTotalSF != 0 {
		t.Errorf("PFTotalSF = %d, want 0", got.PFTotalSF)
	}
}

func TestParseRshimMisc(t *testing.T) {
	in := `DEV_NAME        rshim0
DEV_INFO        BlueField-3(Rev 1)
BF mode         DPU(1)
BOOT_MODE       1
UUID            12345678-1234-5678-1234-567812345678
`
	got := parseRshimMisc(in)
	if got == nil {
		t.Fatal("nil")
	}
	if got["DEV_NAME"] != "rshim0" || got["UUID"] != "12345678-1234-5678-1234-567812345678" {
		t.Errorf("got = %+v", got)
	}
}

func TestParseIPAddrJSON(t *testing.T) {
	in := `[
  {"ifindex":1,"ifname":"lo","mtu":65536,"address":"00:00:00:00:00:00",
   "addr_info":[{"local":"127.0.0.1","prefixlen":8}]},
  {"ifindex":2,"ifname":"eno1","mtu":1500,"address":"aa:bb:cc:dd:ee:ff",
   "addr_info":[{"local":"192.168.1.10","prefixlen":24}]}
]`
	got := parseIPAddrJSON(in)
	if len(got) != 2 {
		t.Fatalf("got %d ifaces, want 2", len(got))
	}
	if got[1].Name != "eno1" || got[1].MTU != 1500 || got[1].MAC != "aa:bb:cc:dd:ee:ff" {
		t.Errorf("eno1 = %+v", got[1])
	}
	if len(got[1].IPs) != 1 || got[1].IPs[0] != "192.168.1.10" {
		t.Errorf("eno1 IPs = %v", got[1].IPs)
	}
}
