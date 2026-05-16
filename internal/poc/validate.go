package poc

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
)

// Phase tags identify the earliest pipeline phase that requires a given
// field. Validate rules are filtered against the operator's intent: if
// they're about to run `provision dpus`, rules tagged with Cluster or
// Deploy don't apply yet, even though the field is still empty. The
// bare `dpubnkctl validate` (and the `validate` step in `e2e`) run with
// the maximum phase so they catch everything.
//
// Tagging convention: a rule's phase is the earliest phase whose
// dpubnkctl subcommand will hard-fail without the value. FAR/JWT only
// matter at deploy; bf.conf needs dpu_mtu at provision; kubespray needs
// pod_mtu at cluster.
type Phase int

const (
	PhaseProvision Phase = 1
	PhaseCluster   Phase = 2
	PhaseDeploy    Phase = 3
)

// String returns the canonical CLI label.
func (p Phase) String() string {
	switch p {
	case PhaseProvision:
		return "provision"
	case PhaseCluster:
		return "cluster"
	case PhaseDeploy:
		return "deploy"
	}
	return "unknown"
}

// ValidationResult is what Validate returns: a list of blocking errors
// and a list of non-blocking warnings.
//
// Errors mean some downstream phase will fail (kubespray plan, bf.conf
// render, helm install). Warnings flag values that are still at template
// defaults or "you might want to think about this" — they don't block
// the binary from running but the SE should journal a decision before
// leaving them as-is.
type ValidationResult struct {
	Errors   []string
	Warnings []string
}

func (r ValidationResult) Valid() bool { return len(r.Errors) == 0 }

// roleRe enforces a VLAN role that combines with the tag into a Linux
// interface name (≤15 chars total). Duplicated from internal/provision
// to keep this package import-free from siblings — they're stable.
var roleRe = regexp.MustCompile(`^[a-z][a-z0-9]{0,9}$`)

// safeNameRe gates strings that end up in shell command lines or
// filesystem paths under artifacts/ — Host.Name, DPU.Hostname. RFC 1123
// label, all lowercase to match k8s node-name rules; this is also what
// dpubnkctl init enforces for the PoC name itself.
var safeNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// safeIfaceRe gates network interface names that flow into `ethtool`,
// `ip link`, and netplan link references. Linux IFNAMSIZ caps at 15
// chars; we additionally disallow whitespace, quotes, command-substitution
// metacharacters — anything that could break out of a shell argv.
var safeIfaceRe = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,15}$`)

// safeBFBNameRe gates the on-disk BFB filename that gets uploaded to
// the host and named in `bfb-install --bfb '<name>'`. Single-quoted in
// the script today, but disallow quote chars entirely so a poc.yaml
// override can't escape.
var safeBFBNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]+\.bfb$`)

// defaultInternalCIDR is the placeholder that ships in `dpubnkctl init`
// — it's a documented-safe RFC 2544 default, but the SE should confirm
// it doesn't overlap the customer's existing ranges.
const defaultInternalCIDR = "198.18.100.0/24"

// checker collects errors/warnings while filtering by phase. Rules call
// c.err / c.warn with the earliest phase that needs them; the checker
// drops anything past minPhase so a `provision dpus` invocation isn't
// blocked by a missing JWT (which only matters at deploy).
type checker struct {
	r        *ValidationResult
	minPhase Phase
}

func (c *checker) err(phase Phase, format string, args ...any) {
	if phase > c.minPhase {
		return
	}
	c.r.Errors = append(c.r.Errors, fmt.Sprintf(format, args...))
}

func (c *checker) warn(phase Phase, format string, args ...any) {
	if phase > c.minPhase {
		return
	}
	c.r.Warnings = append(c.r.Warnings, fmt.Sprintf(format, args...))
}

// Validate walks a loaded PoC and reports every issue Validate can catch
// statically — the equivalent of ValidateForPhase(p, repoDir, PhaseDeploy).
// Used by the bare `dpubnkctl validate` (which has no in-context phase).
func Validate(p *PoC, repoDir string) ValidationResult {
	return ValidateForPhase(p, repoDir, PhaseDeploy)
}

// ValidateForPhase runs only the rules required at or before minPhase.
// Each subcommand precheck calls this with its own phase so we don't
// block `provision dpus` on FAR/JWT files that won't be needed until
// `deploy cne` weeks later.
//
// Validate intentionally errs toward being noisy: each ERROR is a thing
// some phase command will fail-loud on anyway, and each WARNING is a
// thing that has bitten a real PoC at least once.
func ValidateForPhase(p *PoC, repoDir string, minPhase Phase) ValidationResult {
	var r ValidationResult
	c := &checker{r: &r, minPhase: minPhase}

	// --- metadata ---
	if p.Metadata.Name == "" {
		c.err(PhaseProvision, "metadata.name is empty")
	}
	if p.Metadata.Customer == "" {
		c.warn(PhaseDeploy, "metadata.customer is empty — record the customer name for the final report")
	}

	// --- network ---
	if p.Network.InternalCIDR == "" {
		c.err(PhaseCluster, "network.internal_cidr is empty (pod CIDR — must not overlap any data-plane subnet)")
	} else if _, _, err := net.ParseCIDR(p.Network.InternalCIDR); err != nil {
		c.err(PhaseCluster, "network.internal_cidr %q is not a valid CIDR", p.Network.InternalCIDR)
	} else if p.Network.InternalCIDR == defaultInternalCIDR {
		c.warn(PhaseCluster, "network.internal_cidr is still at the template default 198.18.100.0/24 — confirm with the customer it does not overlap any subnet they use elsewhere")
	}
	if p.Network.DPUMTU == 0 {
		c.err(PhaseProvision, "network.dpu_mtu is 0 (set to 9000, or to whatever MTU the customer's switch fabric supports end-to-end)")
	}
	if p.Network.PodMTU == 0 {
		c.err(PhaseCluster, "network.pod_mtu is 0 (set to 8900 for the standard 9000 fabric)")
	}
	if p.Network.PodMTU > p.Network.DPUMTU {
		c.err(PhaseCluster, "network.pod_mtu (%d) > network.dpu_mtu (%d) — pod MTU must be ≤ DPU MTU minus overlay overhead", p.Network.PodMTU, p.Network.DPUMTU)
	}
	if p.Network.ClusterAPIServerAddress == "" {
		c.warn(PhaseCluster, "network.cluster_apiserver_address is empty — externally-joined DPUs need a routable apiserver address; without this kubespray's localhost-nginx-proxy hack takes over and DPUs can't reach the apiserver (see AGENTS.md #4)")
	}
	if p.Network.NodeIPRole == "" {
		c.warn(PhaseCluster, "network.node_ip_role is empty — hosts will fall back to ssh.address (mgmt) for kubelet --node-ip, DPUs auto-detect; usually you want this set to the data-plane role (e.g. \"internal\")")
	}

	// --- hosts (presence + per-host shape) ---
	if len(p.Hosts) == 0 {
		c.err(PhaseProvision, "no hosts in poc.yaml — run `dpubnkctl discover wizard` (or `discover range`) first")
	}
	cps, workers := 0, 0
	for i, h := range p.Hosts {
		hctx := fmt.Sprintf("hosts[%d:%s]", i, h.Name)
		if h.Name == "" {
			c.err(PhaseProvision, "%s.name is empty", hctx)
		} else if !safeNameRe.MatchString(h.Name) {
			// Host.Name flows into artifacts/<name>.pem, bf.conf filenames,
			// and the kubespray inventory — any value that isn't a strict
			// RFC 1123 label can either escape filesystem boundaries or
			// land in a shell command unquoted. See AGENTS.md threat model.
			c.err(PhaseProvision, "%s.name %q must match %s (RFC 1123 label, lowercase)", hctx, h.Name, safeNameRe.String())
		}
		// Role classification is required by kubespray inventory rendering
		// (cluster phase). Provision flashes any host's DPU regardless of
		// role, so we don't enforce it earlier.
		switch h.Role {
		case "":
			c.err(PhaseCluster, "%s.role is empty (set to control-plane | worker | both)", hctx)
		case "control-plane":
			cps++
		case "worker":
			workers++
		case "both":
			cps++
			workers++
		default:
			c.err(PhaseCluster, "%s.role %q invalid (must be control-plane | worker | both)", hctx, h.Role)
		}
		// SSH credentials are needed at provision (to drive bfb-install
		// over rshim via the host) and every subsequent phase.
		if h.SSH.Address == "" {
			c.err(PhaseProvision, "%s.ssh.address is empty", hctx)
		}
		if h.SSH.User == "" {
			c.err(PhaseProvision, "%s.ssh.user is empty", hctx)
		}
		if h.SSH.KeyRef == "" {
			c.err(PhaseProvision, "%s.ssh.key_ref is empty", hctx)
		} else if !fileExists(repoDir, h.SSH.KeyRef) {
			c.err(PhaseProvision, "%s.ssh.key_ref %q file not found (drop the private key into the PoC repo or fix the path)", hctx, h.SSH.KeyRef)
		}

		// Per-DPU. bf.conf needs every field at provision.
		for j, d := range h.DPUs {
			dctx := fmt.Sprintf("%s.dpus[%d:%s]", hctx, j, d.PCI)
			validateDPU(c, &p.Hosts[i].DPUs[j], dctx)
			_ = d
		}

		// Data-plane VLAN sub-interfaces on the host. host network setup
		// runs at the cluster phase, so the data_plane block isn't a
		// provision-time requirement.
		if h.DataPlane != nil {
			if h.DataPlane.ParentIface == "" {
				c.err(PhaseCluster, "%s.data_plane.parent_iface is empty (set to the host's data-plane PF, e.g. ens16f0np0)", hctx)
			} else if !safeIfaceRe.MatchString(h.DataPlane.ParentIface) {
				// ParentIface flows into `ethtool -i %s` and `ip -br addr
				// show dev %s` over SSH. An unquoted value containing a
				// shell metacharacter would execute as the SSH user.
				c.err(PhaseCluster, "%s.data_plane.parent_iface %q must match %s (Linux IFNAMSIZ + shell-safe charset)", hctx, h.DataPlane.ParentIface, safeIfaceRe.String())
			}
			for k, v := range h.DataPlane.VLANs {
				validateHostVLAN(c, v, fmt.Sprintf("%s.data_plane.vlans[%d]", hctx, k))
			}
		} else if len(h.DPUs) > 0 {
			c.warn(PhaseCluster, "%s has DPUs but no data_plane block — host won't have a VLAN sub-interface to talk to the fabric (`dpubnkctl host network setup` will skip it)", hctx)
		}
	}
	if len(p.Hosts) > 0 && cps == 0 {
		c.err(PhaseCluster, "no control-plane hosts (at least one host needs role: control-plane or both)")
	}
	if len(p.Hosts) > 0 && workers == 0 {
		c.err(PhaseCluster, "no worker hosts (at least one host needs role: worker or both)")
	}
	if cps == 2 {
		c.warn(PhaseCluster, "2 control planes is not HA-safe — etcd quorum requires 3 to survive a single failure")
	}

	// Cross-check: with a single control plane, cluster_apiserver_address
	// must equal that host's VLAN-of-role IP. The lake1/homelab PoC
	// caught this the hard way — operator set a placeholder VIP that
	// no listener answered on, kubeadm hung 4 minutes before failing.
	// With >1 CP an external VIP is plausible, so skip the check there.
	if cps == 1 && p.Network.ClusterAPIServerAddress != "" && p.Network.NodeIPRole != "" {
		role := p.Network.NodeIPRole
		addr := p.Network.ClusterAPIServerAddress
		var cpHost *Host
		for i := range p.Hosts {
			h := &p.Hosts[i]
			if h.Role == "control-plane" || h.Role == "both" {
				cpHost = h
				break
			}
		}
		if cpHost != nil {
			v := cpHost.VLANByRole(role)
			if v == nil {
				c.err(PhaseCluster, "network.cluster_apiserver_address %s set, but control-plane host %q has no data_plane vlan with role=%q (network.node_ip_role) — kubeadm will hang trying to reach it", addr, cpHost.Name, role)
			} else if ip, _, err := net.ParseCIDR(v.IP); err == nil && ip.String() != addr {
				c.err(PhaseCluster, "network.cluster_apiserver_address (%s) doesn't match control-plane %q's %s VLAN IP (%s) — with a single control plane, the address must equal that host's data-plane IP, no VIP listener answers otherwise (kubeadm hung 4 min on this in a past PoC)", addr, cpHost.Name, role, ip.String())
			}
		}
	}

	// --- provisioning (everything here renders into bf.conf at provision) ---
	if p.Provisioning.DPUPasswordHashRef == "" {
		c.err(PhaseProvision, "provisioning.dpu_password_hash_ref is empty (path to file containing the output of `openssl passwd -1 '<password>'`)")
	} else if !fileExists(repoDir, p.Provisioning.DPUPasswordHashRef) {
		c.err(PhaseProvision, "provisioning.dpu_password_hash_ref %q file not found", p.Provisioning.DPUPasswordHashRef)
	}
	// BFB filename feeds `sudo bfb-install --bfb '<name>'` on the host;
	// the value is single-quoted but a poc.yaml override with an embedded
	// single quote would escape the quoting. Require a strict
	// filesystem-safe shape with a literal .bfb extension.
	if p.Versions.BFBImage != "" && !safeBFBNameRe.MatchString(p.Versions.BFBImage) {
		c.err(PhaseProvision, "versions.bfb_image %q must match %s", p.Versions.BFBImage, safeBFBNameRe.String())
	}
	if len(p.Provisioning.DPUDNS) == 0 {
		c.err(PhaseProvision, "provisioning.dpu_dns is empty (DPU systemd-resolved needs at least one resolver)")
	}
	if len(p.Provisioning.DPUNTP) == 0 {
		c.err(PhaseProvision, "provisioning.dpu_ntp is empty (DPU chrony needs at least one source)")
	}

	// --- BNK credentials (only required at `deploy flo` / `deploy cne`) ---
	if p.BNK.FARKeyRef == "" {
		c.err(PhaseDeploy, "bnk.far_key_ref is empty (path to the f5-far-auth-key tarball; obtained from F5 license portal)")
	} else if !fileExists(repoDir, p.BNK.FARKeyRef) {
		c.err(PhaseDeploy, "bnk.far_key_ref %q file not found — drop the FAR tgz into keys/", p.BNK.FARKeyRef)
	}
	if p.BNK.JWTRef == "" {
		c.err(PhaseDeploy, "bnk.jwt_ref is empty (path to the TEEM JWT; obtained from F5 license portal)")
	} else if !fileExists(repoDir, p.BNK.JWTRef) {
		c.err(PhaseDeploy, "bnk.jwt_ref %q file not found — drop the JWT into keys/.jwt", p.BNK.JWTRef)
	}
	if p.BNK.ExternalSelfIP == "" {
		c.warn(PhaseDeploy, "bnk.external_selfip is empty — required for the F5SPKVlan that TMM binds for north-south traffic; `deploy cne` will fail without it")
	}
	if p.BNK.InternalSelfIP == "" {
		c.warn(PhaseDeploy, "bnk.internal_selfip is empty — same shape as external_selfip, internal-side")
	}

	return r
}

// validateDPU runs per-DPU checks. Mirrors the field requirements in
// internal/provision.buildInputs (bf.conf render) — all provision-phase.
func validateDPU(c *checker, d *DPU, ctx string) {
	if d.PCI == "" {
		c.err(PhaseProvision, "%s.pci is empty", ctx)
	}
	switch d.Mode {
	case "":
		c.err(PhaseProvision, "%s.mode is empty (set to dpu | nic)", ctx)
	case "dpu", "nic":
		// ok
	default:
		c.err(PhaseProvision, "%s.mode %q invalid (must be dpu | nic)", ctx, d.Mode)
	}
	if d.Hostname == "" {
		c.err(PhaseProvision, "%s.hostname is empty (DPU OS hostname, set before flash)", ctx)
	} else if !safeNameRe.MatchString(d.Hostname) {
		// DPU.Hostname flows into kubeadm join's --node-name and the
		// bf.conf hostname line. Strict RFC 1123 label is also what k8s
		// node-name requires; anything else would break the join.
		c.err(PhaseProvision, "%s.hostname %q must match %s (RFC 1123 label, lowercase)", ctx, d.Hostname, safeNameRe.String())
	}
	if d.TmfifoIP == "" {
		c.err(PhaseProvision, "%s.tmfifo_ip is empty (tmfifo_net0 CIDR, e.g. 192.168.100.2/30)", ctx)
	} else {
		// rshim driver hard-codes the host side at 192.168.100.1/30, so
		// the DPU's tmfifo_net0 must live in the same /30 and not collide
		// with .1. The lake1 PoC hit this: operator picked .6/30, which
		// is a *different* /30 (.4 net / .5 first / .6 second / .7 bcast)
		// — host's rshim auto-took 192.168.100.1/30, ProxyJump SSH broke
		// with "No route to host". Catch the entire failure shape.
		validateTmfifoIP(c, d.TmfifoIP, ctx)
	}
	if len(d.VLANs) == 0 {
		c.warn(PhaseProvision, "%s has no vlans — DPU won't have any data-plane interfaces", ctx)
	}
	for k, v := range d.VLANs {
		vctx := fmt.Sprintf("%s.vlans[%d]", ctx, k)
		validateDPUVLAN(c, v, vctx, d.LAG)
	}
}

// rshimHostIP is the address the BlueField rshim driver auto-assigns
// on the host side. Hard-coded by the kernel module; the operator can
// technically override via sysfs but no PoC has ever done that.
var rshimHostIP = net.ParseIP("192.168.100.1")

// validateTmfifoIP enforces the rshim /30 constraint. Anything that
// would leave the host's rshim interface and the DPU's tmfifo_net0
// in different subnets — or collide on .1 — is an error.
func validateTmfifoIP(c *checker, raw, ctx string) {
	ip, ipnet, err := net.ParseCIDR(raw)
	if err != nil {
		c.err(PhaseProvision, "%s.tmfifo_ip %q is not a valid CIDR (typical: 192.168.100.2/30)", ctx, raw)
		return
	}
	ones, bits := ipnet.Mask.Size()
	if bits != 32 {
		c.err(PhaseProvision, "%s.tmfifo_ip %q must be IPv4", ctx, raw)
		return
	}
	if ones != 30 {
		c.err(PhaseProvision, "%s.tmfifo_ip %q must be a /30 (rshim driver uses /30); typical: 192.168.100.2/30", ctx, raw)
		return
	}
	if !ipnet.Contains(rshimHostIP) {
		c.err(PhaseProvision, "%s.tmfifo_ip %q is on a different /30 than the host rshim default (192.168.100.1/30) — host cannot route to the DPU; use 192.168.100.2/30", ctx, raw)
		return
	}
	if ip.Equal(rshimHostIP) {
		c.err(PhaseProvision, "%s.tmfifo_ip %q collides with the host rshim address 192.168.100.1 — use 192.168.100.2/30", ctx, raw)
		return
	}
	// /30 of 192.168.100.0: .0 (network), .1 (rshim), .2 (DPU usable),
	// .3 (broadcast). Only .2 is a valid DPU address; .0 and .3 are
	// unaddressable.
	last := ip.To4()[3]
	if last != 2 {
		c.err(PhaseProvision, "%s.tmfifo_ip %q must be 192.168.100.2/30 — .0/.3 of the rshim /30 are network/broadcast, .1 is rshim", ctx, raw)
	}
}

func validateDPUVLAN(c *checker, v DPUVLAN, ctx string, lag bool) {
	if !roleRe.MatchString(v.Role) {
		c.err(PhaseProvision, "%s.role %q must match %s (e.g. external, internal, storage)", ctx, v.Role, roleRe.String())
	}
	if v.Tag <= 0 || v.Tag > 4094 {
		c.err(PhaseProvision, "%s.tag %d invalid (must be 1..4094)", ctx, v.Tag)
	}
	if v.IP == "" {
		c.err(PhaseProvision, "%s.ip is empty", ctx)
	} else if _, _, err := net.ParseCIDR(v.IP); err != nil {
		c.err(PhaseProvision, "%s.ip %q is not a valid CIDR", ctx, v.IP)
	}
	if name := v.PortName(); len(name) > 15 {
		c.err(PhaseProvision, "%s derived port name %q exceeds 15 chars (Linux IFNAMSIZ); shorten role", ctx, name)
	}
	if !lag {
		if v.Uplink != "p0" && v.Uplink != "p1" {
			c.err(PhaseProvision, "%s.uplink %q invalid (must be p0 or p1 in non-LAG mode)", ctx, v.Uplink)
		}
	}
}

func validateHostVLAN(c *checker, v HostDataPlaneVLAN, ctx string) {
	if !roleRe.MatchString(v.Role) {
		c.err(PhaseCluster, "%s.role %q must match %s", ctx, v.Role, roleRe.String())
	}
	if v.Tag <= 0 || v.Tag > 4094 {
		c.err(PhaseCluster, "%s.tag %d invalid (must be 1..4094)", ctx, v.Tag)
	}
	if v.IP == "" {
		c.err(PhaseCluster, "%s.ip is empty", ctx)
	} else if _, _, err := net.ParseCIDR(v.IP); err != nil {
		c.err(PhaseCluster, "%s.ip %q is not a valid CIDR", ctx, v.IP)
	}
	if name := v.PortName(); len(name) > 15 {
		c.err(PhaseCluster, "%s derived port name %q exceeds 15 chars (Linux IFNAMSIZ); shorten role", ctx, name)
	}
}

func fileExists(repoDir, ref string) bool {
	if ref == "" {
		return false
	}
	path := ref
	if !filepath.IsAbs(path) {
		path = filepath.Join(repoDir, ref)
	}
	_, err := os.Stat(path)
	return err == nil
}
