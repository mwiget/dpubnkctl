package discover

import (
	"net"
	"testing"
)

func TestParseRange_SingleIP(t *testing.T) {
	got, err := ParseRange("192.168.68.66")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].String() != "192.168.68.66" {
		t.Errorf("got %v", got)
	}
}

func TestParseRange_LastOctet(t *testing.T) {
	got, err := ParseRange("192.168.68.66-71")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 6 {
		t.Fatalf("got %d ips, want 6", len(got))
	}
	if got[0].String() != "192.168.68.66" || got[5].String() != "192.168.68.71" {
		t.Errorf("first/last = %s, %s", got[0], got[5])
	}
}

func TestParseRange_FullRange(t *testing.T) {
	got, err := ParseRange("10.0.0.10-10.0.0.12")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d, want 3", len(got))
	}
	if got[2].String() != "10.0.0.12" {
		t.Errorf("last = %s", got[2])
	}
}

func TestParseRange_CIDR_Slash30(t *testing.T) {
	// /30 has 4 addresses; we strip network + broadcast → 2 hosts.
	got, err := ParseRange("10.0.0.0/30")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d, want 2 hosts in /30", len(got))
	}
	if got[0].String() != "10.0.0.1" || got[1].String() != "10.0.0.2" {
		t.Errorf("got %v %v", got[0], got[1])
	}
}

func TestParseRange_CIDR_Slash31_KeepsAll(t *testing.T) {
	// Point-to-point: don't strip.
	got, err := ParseRange("10.0.0.0/31")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("got %d, want 2", len(got))
	}
}

func TestParseRange_CIDR_Slash32(t *testing.T) {
	got, err := ParseRange("10.0.0.5/32")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].String() != "10.0.0.5" {
		t.Errorf("got %v", got)
	}
}

func TestParseRange_Errors(t *testing.T) {
	cases := []string{"", "not-an-ip", "10.0.0.5-1", "10.0.0.0/40", "192.168.1.1-300"}
	for _, c := range cases {
		if _, err := ParseRange(c); err == nil {
			t.Errorf("expected error for %q", c)
		}
	}
}

func TestEnumerate_Sanity(t *testing.T) {
	got := enumerate(ipToU32(net.IPv4(192, 168, 0, 250).To4()),
		ipToU32(net.IPv4(192, 168, 1, 5).To4()))
	if len(got) != 12 {
		t.Errorf("expected 12 IPs across .250-.255 + .0-.5, got %d", len(got))
	}
	if got[0].String() != "192.168.0.250" || got[11].String() != "192.168.1.5" {
		t.Errorf("bounds wrong: %s..%s", got[0], got[11])
	}
}
