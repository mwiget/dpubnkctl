package poc

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"

	"github.com/mwiget/dpubnkctl/internal/version"
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

// safePCIRe gates DPU.PCI — flows into `mlxconfig -d %s` inside the
// readiness probe and `mlx5_core` driver paths. Linux PCIe BDF accepts
// both the full domain-prefixed form (`DDDD:BB:DD.F`, e.g.
// `0000:03:00.0`) and the short form without the domain
// (`BB:DD.F`, e.g. `00:10.0`). lspci often emits the short form, and
// `mlxconfig -d <bdf>` handles both, so accept either.
var safePCIRe = regexp.MustCompile(`^([0-9a-fA-F]{4}:)?[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-7]$`)

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
	// join_transport selects the DPU join path. rshim suppresses the
	// data-plane-VLAN warnings below (it deliberately doesn't use them).
	rshim := p.Network.IsRshim()
	switch p.Network.JoinTransport {
	case "", JoinTransportVLAN, JoinTransportRshim:
		// ok
	default:
		c.err(PhaseCluster, "network.join_transport %q invalid (must be %q or %q)", p.Network.JoinTransport, JoinTransportVLAN, JoinTransportRshim)
	}
	if !rshim && p.Network.ClusterAPIServerAddress == "" {
		c.warn(PhaseCluster, "network.cluster_apiserver_address is empty — externally-joined DPUs need a routable apiserver address; without this kubespray's localhost-nginx-proxy hack takes over and DPUs can't reach the apiserver (see AGENTS.md #4)")
	}
	if !rshim && p.Network.NodeIPRole == "" {
		c.warn(PhaseCluster, "network.node_ip_role is empty — hosts will fall back to ssh.address (mgmt) for kubelet --node-ip, DPUs auto-detect; usually you want this set to the data-plane role (e.g. \"internal\")")
	}
	// rshim-specific network checks.
	if rshim {
		validateTmfifoPool(c, p)
		if p.Network.ClusterAPIServerAddress != "" {
			c.warn(PhaseCluster, "network.cluster_apiserver_address is set but ignored under rshim join — the DPU reaches the apiserver over tmfifo and the host keeps its mgmt address; clear it to avoid confusion (a leftover from a VLAN-join config)")
		}
	}
	switch p.Provisioning.DPUInternet {
	case "", DPUInternetHostNAT, DPUInternetOOB, DPUInternetNone:
		// ok
	default:
		c.err(PhaseCluster, "provisioning.dpu_internet %q invalid (must be %q, %q or %q)", p.Provisioning.DPUInternet, DPUInternetHostNAT, DPUInternetOOB, DPUInternetNone)
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
			validateDPU(c, &p.Hosts[i].DPUs[j], dctx, p.Network)
			_ = d
		}
		// Per-host tmfifo wiring: one /30 and one rshim interface per DPU.
		validateHostTmfifoLinks(c, &p.Hosts[i], hctx)

		// Multi-DPU-per-host over rshim is still unsupported, but no
		// longer for the addressing reason: distinct /30s per DPU and
		// per-DPU tmfifo_iface (issue #20) made the vlan-transport case
		// work. What rshim additionally needs is the join-time plumbing
		// per link — the host-NAT MASQUERADE source and the DPU default
		// route in setup_dpu_networking are still derived per host, not
		// per tmfifo link. Kept as an explicit error rather than a
		// silent half-working path.
		if rshim && len(h.DPUs) > 1 {
			c.err(PhaseProvision, "%s has %d DPUs but join_transport=rshim supports one DPU per host — the per-link host NAT and DPU default route aren't wired per tmfifo link yet. Use the vlan join transport for multi-DPU hosts (addressing itself is supported there: give each DPU its own tmfifo_ip /30 and tmfifo_iface)", hctx, len(h.DPUs))
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
			validateNonLAGUplinkFanout(c, &p.Hosts[i], hctx)
		} else if len(h.DPUs) > 0 {
			c.warn(PhaseCluster, "%s has DPUs but no data_plane block — host won't have a VLAN sub-interface to talk to the fabric (`dpubnkctl host network setup` will skip it)", hctx)
		}
	}
	// DPU hostnames must be unique across the whole PoC: DPU.Hostname is
	// passed to `kubeadm join --node-name`, and two DPUs sharing a name
	// means the second join silently takes over the first one's Node
	// object — the cluster ends up one node short with no error anywhere,
	// and whichever DPU lost the race keeps running a kubelet that fights
	// for the same registration. It also collides in artifacts/ (per-DPU
	// flash logs are keyed by hostname).
	//
	// Fleet-wide, not per-host: DPUs on different hosts are still distinct
	// k8s nodes. Hit on the Tokyo lab, where both BF3s on one host were
	// defaulted to <host>-bf3 (issue #18).
	seenDPUHostname := map[string]string{} // hostname -> first owner's ctx
	for i := range p.Hosts {
		for j := range p.Hosts[i].DPUs {
			name := p.Hosts[i].DPUs[j].Hostname
			if name == "" {
				continue // already reported by validateDPU
			}
			dctx := fmt.Sprintf("hosts[%d:%s].dpus[%d:%s]", i, p.Hosts[i].Name, j, p.Hosts[i].DPUs[j].PCI)
			if first, dup := seenDPUHostname[name]; dup {
				c.err(PhaseProvision, "%s.hostname %q is already used by %s — DPU hostnames become kubeadm --node-name values and must be unique across the PoC (two DPUs on one host need distinct names, e.g. %s-1 / %s-2)", dctx, name, first, name, name)
				continue
			}
			seenDPUHostname[name] = dctx
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
	if !rshim && cps == 1 && p.Network.ClusterAPIServerAddress != "" && p.Network.NodeIPRole != "" {
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
	if p.Provisioning.BFBOnHost != "" {
		// Must be absolute — provision_dpu passes it verbatim to bfb-install
		// on the host. A relative path could land somewhere unexpected
		// depending on the user's home directory and is almost certainly
		// an operator typo.
		if !filepath.IsAbs(p.Provisioning.BFBOnHost) {
			c.err(PhaseProvision, "provisioning.bfb_on_host %q must be an absolute path on the host (e.g. /var/cache/dpubnkctl/bfb/<name>.bfb)", p.Provisioning.BFBOnHost)
		}
		if p.Provisioning.BFBURL != "" {
			c.warn(PhaseProvision, "provisioning.bfb_on_host is set — provisioning.bfb_url is ignored")
		}
	}

	// --- BFB fetch mode + integrity (provision) ---
	switch p.Provisioning.BFBFetch {
	case "", BFBFetchPush, BFBFetchHost:
		// ok
	default:
		c.err(PhaseProvision, "provisioning.bfb_fetch %q invalid (must be %q or %q)", p.Provisioning.BFBFetch, BFBFetchPush, BFBFetchHost)
	}
	if p.Provisioning.BFBFetch == BFBFetchHost {
		// Mutually exclusive with a manually pre-staged file — one curls
		// the BFB for you, the other reuses what the operator staged.
		if p.Provisioning.BFBOnHost != "" {
			c.err(PhaseProvision, "provisioning.bfb_on_host and bfb_fetch: host are mutually exclusive — unset one")
		}
		// host mode needs a URL to curl from: the poc override or the
		// binary-pinned base. Both empty means there's nothing to fetch.
		if p.Provisioning.BFBURL == "" && p.Versions.BFBURL == "" && version.BFBBaseURL == "" {
			c.err(PhaseProvision, "bfb_fetch: host needs a BFB URL — set versions.bfb_url (no binary-pinned base available)")
		}
		if p.Versions.BFBImage == "" && version.BFBImage == "" {
			c.err(PhaseProvision, "bfb_fetch: host needs versions.bfb_image set (nothing to fetch)")
		}
	}
	if p.Provisioning.BFBHostCacheDir != "" && !filepath.IsAbs(p.Provisioning.BFBHostCacheDir) {
		// Flows into `mkdir -p`/`curl -o` on the host; a relative path
		// would land in the SSH user's home rather than the intended
		// system cache dir.
		c.err(PhaseProvision, "provisioning.bfb_host_cache_dir %q must be an absolute path on the host (default %s)", p.Provisioning.BFBHostCacheDir, DefaultBFBHostCacheDir)
	}
	// Integrity: warn once when no digest is known for any fetch mode.
	// Precedence mirrors provision.ExpectedBFBSHA256 (kept in sync here to
	// avoid poc→provision import): poc override > binary pin.
	if p.Provisioning.BFBSHA256 == "" && version.BFBImageSHA256 == "" {
		c.warn(PhaseProvision, "no BFB sha256 is pinned (version pin empty, provisioning.bfb_sha256 unset) — BFB integrity will not be enforced for any fetch mode")
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
func validateDPU(c *checker, d *DPU, ctx string, netw Network) {
	if d.PCI == "" {
		c.err(PhaseProvision, "%s.pci is empty", ctx)
	} else if !safePCIRe.MatchString(d.PCI) {
		// d.PCI is interpolated into `mlxconfig -d %s` inside the
		// readiness probe; a poc.yaml planted with metacharacters would
		// run arbitrary commands as the SSH user. Canonical BDF shape
		// also serves as a syntax check — typos like missing the domain
		// prefix get caught here rather than at readiness time.
		c.err(PhaseProvision, "%s.pci %q must match %s (canonical PCIe BDF, e.g. 0000:03:00.0)", ctx, d.PCI, safePCIRe.String())
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
	switch {
	case d.TmfifoIP == "" && netw.IsRshim():
		// Under rshim the tmfifo IP is allocated at provision time (from
		// network.tmfifo_cidr, or the 192.168.100.2/30 default) and
		// persisted back into poc.yaml — so an empty value here is fine;
		// AllocateTmfifo fills it before bf.conf render.
	case d.TmfifoIP == "":
		c.err(PhaseProvision, "%s.tmfifo_ip is empty (tmfifo_net0 CIDR, e.g. 192.168.100.2/30)", ctx)
	case netw.IsRshim() && netw.TmfifoCIDR != "":
		// Pool-allocated: the DPU tmfifo IP must be the .2 of a /30 that
		// sits inside network.tmfifo_cidr.
		validateTmfifoIPInPool(c, d.TmfifoIP, netw.TmfifoCIDR, ctx)
	default:
		// vlan transport, or rshim without a pool: the rshim driver
		// hard-codes the host side at 192.168.100.1/30, so the DPU's
		// tmfifo_net0 must live in the same /30 and not collide with .1.
		// The lake1 PoC hit this: operator picked .6/30 while the host's
		// rshim kept 192.168.100.1/30, so the two ends sat in different
		// /30s and ProxyJump SSH broke with "No route to host".
		//
		// A .6/30 DPU is now legal — but only because the host side is
		// derived from the DPU's /30 (DPU.TmfifoHostIP) and applied by
		// ensureHostTmfifoForDPU, so the host follows the DPU onto .5/30
		// instead of being left behind on the rshim default. That
		// derivation is what makes multi-DPU hosts addressable at all
		// (issue #20). validateTmfifoIP still pins the DPU to the second
		// usable address so the two ends can never disagree.
		validateTmfifoIP(c, d.TmfifoIP, ctx)
	}
	// tmfifo_iface reaches `ip link set %s up` and `ip addr add ... dev
	// %s` over SSH — same shell-safety argument as data_plane.parent_iface.
	if d.TmfifoIface != "" && !safeIfaceRe.MatchString(d.TmfifoIface) {
		c.err(PhaseProvision, "%s.tmfifo_iface %q must match %s (Linux IFNAMSIZ + shell-safe charset)", ctx, d.TmfifoIface, safeIfaceRe.String())
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

// validateTmfifoIP enforces the point-to-point /30 shape of a tmfifo
// link: the DPU must sit on the SECOND usable address of its own /30,
// leaving the first for the host side (DPU.TmfifoHostIP derives it).
//
// It deliberately does NOT require the 192.168.100.x block. The rshim
// driver defaults there, and a single-DPU host should keep using
// 192.168.100.2/30 — but pinning every DPU to that block made multi-DPU
// hosts impossible to express: both cards were forced onto the same
// address, so dpubnkctl addressed one DPU twice, and an operator who
// hand-assigned a distinct /30 to the second card got a validation
// error for their trouble (issue #20). Any /30 is accepted; the caller
// separately checks that DPUs on the same host don't overlap.
//
// A DPU that is off the rshim default block only works if the host side
// is moved with it — which ensureHostTmfifoForDPU does, using the same
// derivation. Hence "second usable address" rather than "any address":
// the two ends must agree without a second field to keep in sync.
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
		c.err(PhaseProvision, "%s.tmfifo_ip %q must be a /30 (the tmfifo link is point-to-point; rshim uses /30); typical: 192.168.100.2/30", ctx, raw)
		return
	}
	// In a /30: .0 network, .1 host side, .2 DPU side, .3 broadcast.
	base := ipnet.IP.To4()
	want := net.IPv4(base[0], base[1], base[2], base[3]+2)
	if !ip.Equal(want) {
		c.err(PhaseProvision, "%s.tmfifo_ip %q must be the second usable address of its /30 (%s) — the first (%s.%d) is the host side of the link, and .0/.3 are network/broadcast",
			ctx, raw, want.String(), fmt.Sprintf("%d.%d.%d", base[0], base[1], base[2]), base[3]+1)
	}
}

// validateHostTmfifoLinks checks the per-host tmfifo wiring: every DPU on
// a host needs its own /30 and its own rshim interface.
//
// Each BlueField presents a separate rshim device, so a two-DPU host has
// tmfifo_net0 and tmfifo_net1. Two DPUs sharing a subnet would leave the
// host with two interfaces in one subnet (routing to the DPU address is
// then undefined), and two DPUs sharing an address are indistinguishable
// to dpubnkctl entirely: dpuSSHConfig targets a DPU purely by its tmfifo
// IP, so the second DPU is silently never flashed and never joined while
// every operation "succeeds" against the first (issue #20).
//
// Scoped per host, not fleet-wide: each host↔DPU tmfifo link is a private
// point-to-point segment, so reusing 192.168.100.2/30 on a different host
// is correct and is what every single-DPU PoC does.
func validateHostTmfifoLinks(c *checker, h *Host, hctx string) {
	if len(h.DPUs) < 2 {
		return // a single DPU can't collide with itself
	}
	type seenLink struct {
		ctx   string
		ipnet *net.IPNet
	}
	var links []seenLink
	seenIface := map[string]string{}

	for j := range h.DPUs {
		d := &h.DPUs[j]
		dctx := fmt.Sprintf("%s.dpus[%d:%s]", hctx, j, d.PCI)

		iface := d.TmfifoNetIface()
		if first, dup := seenIface[iface]; dup {
			c.err(PhaseProvision, "%s.tmfifo_iface %q is already used by %s — each BlueField has its own rshim device, so a multi-DPU host needs one interface per DPU (tmfifo_net0, tmfifo_net1, ...). Read the mapping off the host with: for r in /dev/rshim*; do echo \"$r: $(sudo cat $r/misc | grep DEV_NAME)\"; done", dctx, iface, first)
		} else {
			seenIface[iface] = dctx
		}

		if d.TmfifoIP == "" {
			continue // reported elsewhere
		}
		_, ipnet, err := net.ParseCIDR(d.TmfifoIP)
		if err != nil {
			continue // malformed value already reported by validateTmfifoIP
		}
		for _, prev := range links {
			if prev.ipnet.Contains(ipnet.IP) || ipnet.Contains(prev.ipnet.IP) {
				c.err(PhaseProvision, "%s.tmfifo_ip %q overlaps the /30 already used by %s — two DPUs on one host each need their own tmfifo /30 (e.g. 192.168.100.2/30 and 192.168.100.6/30). dpubnkctl reaches a DPU by its tmfifo address alone, so sharing one means the second DPU is never flashed or joined while every step reports success against the first", dctx, d.TmfifoIP, prev.ctx)
				break
			}
		}
		links = append(links, seenLink{ctx: dctx, ipnet: ipnet})
	}
}

// validateTmfifoPool checks network.tmfifo_cidr (rshim multi-host pool)
// and that it holds enough /30 blocks for the fleet's DPUs.
func validateTmfifoPool(c *checker, p *PoC) {
	if p.Network.TmfifoCIDR == "" {
		// Poolless rshim = single-host default. Allowed only for one host
		// with DPUs; AllocateTmfifo enforces the same rule at provision.
		hostsWithDPUs := 0
		for i := range p.Hosts {
			if len(p.Hosts[i].DPUs) > 0 {
				hostsWithDPUs++
			}
		}
		if hostsWithDPUs > 1 {
			c.err(PhaseProvision, "join_transport=rshim with %d hosts needs network.tmfifo_cidr (e.g. 192.168.0.0/24) — the rshim default 192.168.100.x /30 only addresses one host", hostsWithDPUs)
		}
		return
	}
	_, pool, err := net.ParseCIDR(p.Network.TmfifoCIDR)
	if err != nil {
		c.err(PhaseProvision, "network.tmfifo_cidr %q is not a valid CIDR (e.g. 192.168.0.0/24)", p.Network.TmfifoCIDR)
		return
	}
	if ip4 := pool.IP.To4(); ip4 == nil {
		c.err(PhaseProvision, "network.tmfifo_cidr %q must be IPv4", p.Network.TmfifoCIDR)
		return
	}
	ones, _ := pool.Mask.Size()
	if ones > 30 {
		c.err(PhaseProvision, "network.tmfifo_cidr %q is smaller than a /30 — cannot carve a DPU link from it", p.Network.TmfifoCIDR)
		return
	}
	capacity := 1 << uint(30-ones)
	dpus := 0
	for i := range p.Hosts {
		dpus += len(p.Hosts[i].DPUs)
	}
	if dpus > capacity {
		c.err(PhaseProvision, "network.tmfifo_cidr %q holds %d /30 blocks but the fleet has %d DPUs — widen the pool", p.Network.TmfifoCIDR, capacity, dpus)
	}
}

// validateTmfifoIPInPool checks a pool-allocated DPU tmfifo IP: it must be
// a /30, sit inside the pool, and be the .2 (second usable) of its /30 —
// matching AllocateTmfifo's carving (host .1, DPU .2).
func validateTmfifoIPInPool(c *checker, raw, poolCIDR, ctx string) {
	ip, ipnet, err := net.ParseCIDR(raw)
	if err != nil {
		c.err(PhaseProvision, "%s.tmfifo_ip %q is not a valid CIDR", ctx, raw)
		return
	}
	if ones, bits := ipnet.Mask.Size(); bits != 32 || ones != 30 {
		c.err(PhaseProvision, "%s.tmfifo_ip %q must be a /30 (one rshim link per DPU)", ctx, raw)
		return
	}
	_, pool, err := net.ParseCIDR(poolCIDR)
	if err != nil {
		return // pool error already reported by validateTmfifoPool
	}
	if !pool.Contains(ip) {
		c.err(PhaseProvision, "%s.tmfifo_ip %q is outside network.tmfifo_cidr %q", ctx, raw, poolCIDR)
		return
	}
	if last := ip.To4()[3]; last%4 != 2 {
		c.err(PhaseProvision, "%s.tmfifo_ip %q must be the .2 of its /30 (host takes .1) — re-run provision to re-allocate", ctx, raw)
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
	// Same shell-safety argument as data_plane.parent_iface: the value
	// reaches `ethtool -i %s` and `ip -br addr show dev %s` over SSH.
	if v.ParentIface != "" && !safeIfaceRe.MatchString(v.ParentIface) {
		c.err(PhaseCluster, "%s.parent_iface %q must match %s (Linux IFNAMSIZ + shell-safe charset)", ctx, v.ParentIface, safeIfaceRe.String())
	}
}

// bridgeForUplink names the OVS bridge a non-LAG uplink feeds. Mirrors
// the bf.conf template, which routes anything that isn't p1 to
// sf-external. Used only for error text.
func bridgeForUplink(uplink string) string {
	if uplink == "p1" {
		return "sf-internal"
	}
	return "sf-external"
}

// validateNonLAGUplinkFanout enforces the host-PF ↔ DPU-uplink bijection
// on non-LAG DPUs.
//
// A non-LAG DPU runs two OVS bridges — sf-external (uplink p0, fed by
// pf0hpf) and sf-internal (uplink p1, fed by pf1hpf). The host side of
// that is a BIJECTION: host PF0 reaches sf-external and nothing else,
// host PF1 reaches sf-internal and nothing else. The eswitch delivers
// everything arriving on a host PF to that PF's representor **regardless
// of VLAN tag** — tags do not steer between bridges.
//
// So there are two ways to break it, and both are real bugs:
//
//	A. One host PF serves two uplinks. The VLAN whose uplink doesn't
//	   match that PF's bridge is stranded — it leaves via the wrong
//	   uplink or is dropped. This is the reported Tokyo-lab failure.
//	B. One uplink is served by two host PFs. Since each PF reaches
//	   exactly one bridge, one of those VLANs is wired to the wrong
//	   bridge. Same defect, seen from the other side.
//
// Both are invisible locally: the sub-interface comes up, carries its
// IP, and has the right MTU, so `host network setup` reports success.
// Only end-to-end traffic fails, which is an expensive way to find out.
//
// Checked per VLAN, not per uplink. An earlier version kept one
// representative parent per uplink and skipped VLANs that disagreed with
// it, which let a third VLAN pinned to the wrong PF hide behind a
// correctly-mapped pair — the check silently passed the exact shape it
// exists to catch. Iterating d.VLANs in declaration order also keeps the
// reported pair stable run to run (ranging a map made the error text
// flip between runs).
//
// LAG DPUs are exempt: one bridge, one host PF is correct there.
// Reported once per host, at cluster phase — that's when
// `host network setup` renders the netplan. Tokyo lab
// tky-bnk-dpu-host-2, 2026-07-29 (issue #18). See AGENTS.md #33.
func validateNonLAGUplinkFanout(c *checker, h *Host, hctx string) {
	if h.DataPlane == nil || len(h.DataPlane.VLANs) == 0 {
		return
	}
	type claim struct{ role, uplink, parent string }

	for j := range h.DPUs {
		d := &h.DPUs[j]
		if d.LAG || len(d.VLANs) == 0 {
			continue
		}
		dctx := fmt.Sprintf("%s.dpus[%d:%s]", hctx, j, d.PCI)
		byParent := map[string]claim{} // host PF   -> first VLAN that claimed it
		byUplink := map[string]claim{} // DPU uplink -> first VLAN that claimed it

		for _, dv := range d.VLANs {
			uplink := dv.Uplink
			if uplink == "" {
				uplink = "p0" // bf.conf template: anything not p1 rides sf-external
			}
			hv := h.VLANByRole(dv.Role)
			if hv == nil {
				continue // host doesn't carry this VLAN; nothing to map
			}
			cur := claim{role: dv.Role, uplink: uplink, parent: h.DataPlane.ParentFor(*hv)}

			// A: this PF already serves a different uplink.
			if prev, ok := byParent[cur.parent]; ok && prev.uplink != cur.uplink {
				c.err(PhaseCluster,
					"%s is non-LAG: host VLANs %q (DPU uplink %s → %s) and %q (uplink %s → %s) both hang off parent_iface %q, but a host PF reaches exactly one OVS bridge (PF0→pf0hpf→sf-external, PF1→pf1hpf→sf-internal) — the eswitch delivers ALL traffic from a host PF to that PF's bridge regardless of VLAN tag, so the %q VLAN never reaches %s. The sub-interface still comes up with its IP and MTU, so only end-to-end traffic fails. Give each uplink its own host PF via data_plane.vlans[].parent_iface (put %q on the PF wired to %s), or move both VLANs onto one uplink",
					dctx, prev.role, prev.uplink, bridgeForUplink(prev.uplink), cur.role, cur.uplink, bridgeForUplink(cur.uplink),
					cur.parent, cur.role, bridgeForUplink(cur.uplink), cur.role, bridgeForUplink(cur.uplink))
				return
			}
			// B: this uplink is already served by a different PF.
			if prev, ok := byUplink[cur.uplink]; ok && prev.parent != cur.parent {
				c.err(PhaseCluster,
					"%s is non-LAG: host VLANs %q and %q are both on DPU uplink %s (bridge %s) but hang off different parent_ifaces (%q and %q) — each host PF reaches exactly one bridge, so one of these VLANs is wired to the wrong one. Put both on the PF that feeds %s, or move one to the uplink matching its PF",
					dctx, prev.role, cur.role, cur.uplink, bridgeForUplink(cur.uplink),
					prev.parent, cur.parent, bridgeForUplink(cur.uplink))
				return
			}
			byParent[cur.parent] = cur
			byUplink[cur.uplink] = cur
		}
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
