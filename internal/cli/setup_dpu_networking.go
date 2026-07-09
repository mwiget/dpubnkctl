package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/mwiget/dpubnkctl/internal/ssh"
)

// setup_dpu_networking ports the D-020 `bare-metal/setup-dpu-networking`
// module (_execute_tmfifo) to dpubnkctl. It gives the DPU internet access
// through the host over tmfifo so the join-time apt install of
// kubelet/kubeadm/kubectl can reach pkgs.k8s.io. It runs between
// "DPU is tmfifo-reachable" and "install kube binaries" inside joinOneDPU.
//
// Divergences from D-020, per the rshim-join design
// (docs/specs/dpubnkctl-rshim-join-topology.md):
//   - No DPU host-route to the apiserver: dpubnkctl advertises the
//     apiserver on the host's tmfifo IP, which the DPU reaches directly
//     over tmfifo — so the extra `ip route <host_ip>/32 via .1` D-020
//     needs (apiserver on host mgmt IP) is unnecessary here.
//   - Persistence: the host NAT and DPU route/DNS are installed as
//     systemd oneshot units so they survive the warm reboot that happens
//     during provisioning (D-020 applied them runtime-only).
//
// The three wiring alternatives for *where* this runs (in-join pre-step,
// standalone command, end-of-provision) are documented in the design doc;
// this build folds it into join so the apt install always has internet.

const (
	hostNATUnit   = "dpubnkctl-tmfifo-nat"
	dpuRouteUnit  = "dpubnkctl-tmfifo-route"
	hostNATScript = "/usr/local/sbin/dpubnkctl-tmfifo-nat.sh"
	dpuRouteScrpt = "/usr/local/sbin/dpubnkctl-tmfifo-route.sh"
)

// setupDPUNetworking configures host NAT + DPU route/DNS for one DPU.
//   - mode "host-nat": full setup (host MASQUERADE, DPU default route +
//     resolv.conf), verify `ping 8.8.8.8` on the DPU.
//   - mode "oob": no NAT; the DPU already has internet on oob_net0 — just
//     verify reachability and warn if it fails.
//   - mode "none": skip entirely.
//
// hostGatewayIP is the bare host-side tmfifo IP the DPU routes through
// (e.g. 192.168.100.1). masqSrc is the tmfifo subnet to MASQUERADE (the
// pool, or 192.168.100.0/24 for the single-host default). dnsServers are
// written to the DPU's /etc/resolv.conf.
func setupDPUNetworking(ctx context.Context, hostC, dpuC *ssh.Client, mode, hostGatewayIP, masqSrc string, dnsServers []string, w io.Writer) error {
	switch mode {
	case "", "none":
		fmt.Fprintln(w, "setup-dpu-networking: skipped (dpu_internet=none)")
		return nil
	case "oob":
		fmt.Fprintln(w, "setup-dpu-networking: oob mode — DPU uses its own oob_net0 internet; verifying ...")
		if err := verifyDPUInternet(ctx, dpuC); err != nil {
			fmt.Fprintf(w, "setup-dpu-networking: WARN: DPU internet check failed under oob mode: %v\n", err)
		}
		return nil
	case "host-nat":
		// fall through to the full setup below
	default:
		return fmt.Errorf("setup-dpu-networking: unknown dpu_internet mode %q", mode)
	}

	if hostGatewayIP == "" {
		return fmt.Errorf("setup-dpu-networking: empty host tmfifo gateway IP")
	}
	if masqSrc == "" {
		masqSrc = "192.168.100.0/24"
	}
	if len(dnsServers) == 0 {
		dnsServers = []string{"8.8.8.8", "1.1.1.1"}
	}

	// ── Host: ip_forward + MASQUERADE, persisted via a systemd unit ──
	fmt.Fprintf(w, "setup-dpu-networking: host NAT (MASQUERADE %s), persisting ...\n", masqSrc)
	if err := installHostNAT(ctx, hostC, masqSrc); err != nil {
		return fmt.Errorf("host NAT: %w", err)
	}

	// ── DPU: default route via host tmfifo IP + resolv.conf, persisted ──
	fmt.Fprintf(w, "setup-dpu-networking: DPU default route via %s + DNS, persisting ...\n", hostGatewayIP)
	if err := installDPURoute(ctx, dpuC, hostGatewayIP, dnsServers); err != nil {
		return fmt.Errorf("dpu route/dns: %w", err)
	}

	// ── Verify ──
	if err := verifyDPUInternet(ctx, dpuC); err != nil {
		return fmt.Errorf("DPU cannot reach the internet after NAT/routing setup (check host iptables, ip_forward, DPU route): %w", err)
	}
	fmt.Fprintln(w, "setup-dpu-networking: DPU internet verified (ping 8.8.8.8 ok)")
	return nil
}

// installHostNAT writes + enables the host NAT systemd oneshot unit. The
// unit detects the default interface at run time (robust to renames) and
// applies ip_forward + an idempotent MASQUERADE rule. `enable --now` both
// starts it immediately and arms it for the post-provision reboot.
func installHostNAT(ctx context.Context, hostC *ssh.Client, masqSrc string) error {
	script := fmt.Sprintf(`#!/bin/sh
set -e
sysctl -w net.ipv4.ip_forward=1
IFACE=$(ip route | awk '/^default/{print $5; exit}')
[ -n "$IFACE" ] || { echo "setup-dpu-net: no default interface"; exit 1; }
iptables -t nat -C POSTROUTING -s %[1]s -o "$IFACE" -j MASQUERADE 2>/dev/null || \
  iptables -t nat -A POSTROUTING -s %[1]s -o "$IFACE" -j MASQUERADE
`, masqSrc)

	unit := fmt.Sprintf(`[Unit]
Description=dpubnkctl tmfifo NAT (DPU internet via host)
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=%s

[Install]
WantedBy=multi-user.target
`, hostNATScript)

	return writeUnitAndEnable(ctx, hostC, hostNATScript, script, hostNATUnit, unit)
}

// installDPURoute writes + enables the DPU route/DNS systemd oneshot unit.
func installDPURoute(ctx context.Context, dpuC *ssh.Client, gatewayIP string, dnsServers []string) error {
	var resolv strings.Builder
	for _, s := range dnsServers {
		fmt.Fprintf(&resolv, "nameserver %s\n", s)
	}
	script := fmt.Sprintf(`#!/bin/sh
set -e
ip route replace default via %s
cat > /etc/resolv.conf <<'RESOLV'
%sRESOLV
`, gatewayIP, resolv.String())

	unit := fmt.Sprintf(`[Unit]
Description=dpubnkctl tmfifo route + DNS (DPU internet via host)
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=%s

[Install]
WantedBy=multi-user.target
`, dpuRouteScrpt)

	return writeUnitAndEnable(ctx, dpuC, dpuRouteScrpt, script, dpuRouteUnit, unit)
}

// writeUnitAndEnable drops a helper script + its systemd unit onto a node
// and `enable --now`s it. Content is fed through a quoted heredoc so no
// stdin is required. Errors carry stderr for diagnosis.
func writeUnitAndEnable(ctx context.Context, c *ssh.Client, scriptPath, scriptBody, unitName, unitBody string) error {
	unitPath := "/etc/systemd/system/" + unitName + ".service"
	cmd := strings.Join([]string{
		teeHeredoc(scriptPath, scriptBody),
		"sudo -n chmod 0755 " + scriptPath,
		teeHeredoc(unitPath, unitBody),
		"sudo -n systemctl daemon-reload",
		"sudo -n systemctl enable --now " + unitName + ".service",
	}, " && ")
	if r := c.Run(ctx, cmd); !r.OK() {
		return fmt.Errorf("install %s: exit=%d stderr=%s", unitName, r.ExitCode, strings.TrimSpace(r.Stderr))
	}
	return nil
}

// teeHeredoc builds a `sudo tee <path>` fed by a quoted heredoc — the
// body is embedded in the command string, so it needs no SSH stdin. The
// 'DPUBNKEOF' delimiter is single-quoted so the body is written verbatim
// (no shell expansion of $IFACE etc.).
func teeHeredoc(path, body string) string {
	return "sudo -n tee " + path + " >/dev/null <<'DPUBNKEOF'\n" + body + "DPUBNKEOF"
}

// verifyDPUInternet runs a single ping to 8.8.8.8 from the DPU.
func verifyDPUInternet(ctx context.Context, dpuC *ssh.Client) error {
	if r := dpuC.Run(ctx, "ping -c1 -W5 8.8.8.8"); !r.OK() {
		return fmt.Errorf("ping -c1 8.8.8.8 exit=%d stderr=%s", r.ExitCode, strings.TrimSpace(r.Stderr))
	}
	return nil
}
