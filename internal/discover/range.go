package discover

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mwiget/dpubnkctl/internal/ssh"
)

// ParseRange accepts:
//
//	"192.168.68.66"            single IP
//	"192.168.68.66-71"         last-octet shorthand
//	"192.168.68.66-192.168.68.71"  full from-to
//	"192.168.68.0/24"          CIDR
//
// IPv4 only. Returns IPs in ascending order, deduplicated.
func ParseRange(s string) ([]net.IP, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("empty range")
	}

	if strings.Contains(s, "/") {
		return parseCIDR(s)
	}

	if strings.Contains(s, "-") {
		return parseDashRange(s)
	}

	ip := net.ParseIP(s)
	if ip == nil || ip.To4() == nil {
		return nil, fmt.Errorf("not a valid IPv4 address: %q", s)
	}
	return []net.IP{ip.To4()}, nil
}

func parseCIDR(s string) ([]net.IP, error) {
	_, ipnet, err := net.ParseCIDR(s)
	if err != nil {
		return nil, fmt.Errorf("parse CIDR %q: %w", s, err)
	}
	if ipnet.IP.To4() == nil {
		return nil, fmt.Errorf("only IPv4 CIDRs supported, got %q", s)
	}
	start := ipToU32(ipnet.IP.To4())
	mask := ipToU32(net.IP(ipnet.Mask).To4())
	end := start | ^mask
	// Skip network and broadcast for /24 and larger; for /31 and /32 keep all.
	prefixOnes, _ := ipnet.Mask.Size()
	if prefixOnes <= 30 {
		start++
		end--
	}
	return enumerate(start, end), nil
}

func parseDashRange(s string) ([]net.IP, error) {
	left, right, _ := strings.Cut(s, "-")
	left = strings.TrimSpace(left)
	right = strings.TrimSpace(right)

	startIP := net.ParseIP(left)
	if startIP == nil || startIP.To4() == nil {
		return nil, fmt.Errorf("invalid start of range %q", left)
	}
	startIP = startIP.To4()

	var endIP net.IP
	if strings.Contains(right, ".") {
		endIP = net.ParseIP(right)
		if endIP == nil || endIP.To4() == nil {
			return nil, fmt.Errorf("invalid end of range %q", right)
		}
		endIP = endIP.To4()
	} else {
		// Last-octet shorthand: 192.168.68.66-71 → end = 192.168.68.71
		n, err := strconv.Atoi(right)
		if err != nil || n < 0 || n > 255 {
			return nil, fmt.Errorf("invalid last-octet end %q", right)
		}
		endIP = net.IPv4(startIP[0], startIP[1], startIP[2], byte(n)).To4()
	}

	start := ipToU32(startIP)
	end := ipToU32(endIP)
	if end < start {
		return nil, fmt.Errorf("range end %s is before start %s", endIP, startIP)
	}
	return enumerate(start, end), nil
}

func enumerate(start, end uint32) []net.IP {
	out := make([]net.IP, 0, end-start+1)
	for i := start; i <= end; i++ {
		out = append(out, u32ToIP(i))
		if i == ^uint32(0) {
			break
		}
	}
	return out
}

func ipToU32(ip net.IP) uint32 { return binary.BigEndian.Uint32(ip.To4()) }

func u32ToIP(n uint32) net.IP {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, n)
	return net.IP(b)
}

// ScanRange concurrently probes every IP in `ips`. For each that accepts
// SSH within DialTimeout, runs the full host probe battery and emits a
// ScanItem on the returned channel. Unreachable IPs emit a ScanItem with
// Reachable=false and a short Reason.
//
// `Concurrency` caps the number of in-flight probes (default 8).
type ScanOptions struct {
	BaseSSH     ssh.Config // template — Address overridden per IP
	DialTimeout time.Duration
	ProbeTimeout time.Duration
	Concurrency int
}

type ScanItem struct {
	IP        net.IP
	Reachable bool
	Reason    string  // populated when Reachable == false
	Result    *Result // populated when Reachable == true
	Err       error
}

func ScanRange(ctx context.Context, ips []net.IP, opts ScanOptions) <-chan ScanItem {
	if opts.Concurrency <= 0 {
		opts.Concurrency = 8
	}
	if opts.DialTimeout == 0 {
		opts.DialTimeout = 4 * time.Second
	}
	if opts.ProbeTimeout == 0 {
		opts.ProbeTimeout = 60 * time.Second
	}

	out := make(chan ScanItem)
	go func() {
		defer close(out)
		sem := make(chan struct{}, opts.Concurrency)
		var wg sync.WaitGroup
		for _, ip := range ips {
			ip := ip
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				out <- probeOne(ctx, ip, opts)
			}()
		}
		wg.Wait()
	}()
	return out
}

func probeOne(ctx context.Context, ip net.IP, opts ScanOptions) ScanItem {
	cfg := opts.BaseSSH
	cfg.Address = ip.String()
	cfg.Timeout = opts.DialTimeout

	dialCtx, cancel := context.WithTimeout(ctx, opts.DialTimeout)
	defer cancel()

	client, err := ssh.Dial(dialCtx, cfg)
	if err != nil {
		return ScanItem{IP: ip, Reachable: false, Reason: shortenSSHErr(err)}
	}
	defer client.Close()

	probeCtx, pcancel := context.WithTimeout(ctx, opts.ProbeTimeout)
	defer pcancel()

	r, derr := DiscoverHost(probeCtx, HostOptions{Address: ip.String(), Runner: client})
	if derr != nil {
		return ScanItem{IP: ip, Reachable: true, Err: derr}
	}
	return ScanItem{IP: ip, Reachable: true, Result: r}
}

// shortenSSHErr returns a one-line reason for unreachable IPs without
// leaking full error chains into the per-line scan output.
func shortenSSHErr(err error) string {
	s := err.Error()
	switch {
	case strings.Contains(s, "i/o timeout"):
		return "ssh dial: timeout"
	case strings.Contains(s, "connection refused"):
		return "ssh dial: connection refused"
	case strings.Contains(s, "no route to host"):
		return "ssh dial: no route to host"
	case strings.Contains(s, "unable to authenticate"):
		return "ssh auth: rejected"
	case strings.Contains(s, "handshake failed"):
		return "ssh handshake failed"
	}
	if i := strings.Index(s, ": "); i > 0 && i < 60 {
		return s[:i+2] + truncate(s[i+2:], 80)
	}
	return truncate(s, 100)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
