package poc

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
)

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

// defaultInternalCIDR is the placeholder that ships in `dpubnkctl init`
// — it's a documented-safe RFC 2544 default, but the SE should confirm
// it doesn't overlap the customer's existing ranges.
const defaultInternalCIDR = "198.18.100.0/24"

// Validate walks a loaded PoC and reports every issue Validate can catch
// statically. repoDir is the PoC repo root, used to verify that file
// refs (`keys/.jwt`, FAR tgz, SSH keys, password hash) exist.
//
// Validate intentionally errs toward being noisy: each ERROR is a thing
// some phase command will fail-loud on anyway, and each WARNING is a
// thing that has bitten a real PoC at least once.
func Validate(p *PoC, repoDir string) ValidationResult {
	var r ValidationResult

	// --- metadata ---
	if p.Metadata.Name == "" {
		r.Errors = append(r.Errors, "metadata.name is empty")
	}
	if p.Metadata.Customer == "" {
		r.Warnings = append(r.Warnings, "metadata.customer is empty — record the customer name for the final report")
	}

	// --- network ---
	if p.Network.InternalCIDR == "" {
		r.Errors = append(r.Errors, "network.internal_cidr is empty (pod CIDR — must not overlap any data-plane subnet)")
	} else if _, _, err := net.ParseCIDR(p.Network.InternalCIDR); err != nil {
		r.Errors = append(r.Errors, fmt.Sprintf("network.internal_cidr %q is not a valid CIDR", p.Network.InternalCIDR))
	} else if p.Network.InternalCIDR == defaultInternalCIDR {
		r.Warnings = append(r.Warnings, "network.internal_cidr is still at the template default 198.18.100.0/24 — confirm with the customer it does not overlap any subnet they use elsewhere")
	}
	if p.Network.DPUMTU == 0 {
		r.Errors = append(r.Errors, "network.dpu_mtu is 0 (set to 9000, or to whatever MTU the customer's switch fabric supports end-to-end)")
	}
	if p.Network.PodMTU == 0 {
		r.Errors = append(r.Errors, "network.pod_mtu is 0 (set to 8900 for the standard 9000 fabric)")
	}
	if p.Network.PodMTU > p.Network.DPUMTU {
		r.Errors = append(r.Errors, fmt.Sprintf("network.pod_mtu (%d) > network.dpu_mtu (%d) — pod MTU must be ≤ DPU MTU minus overlay overhead", p.Network.PodMTU, p.Network.DPUMTU))
	}
	if p.Network.ClusterAPIServerAddress == "" {
		r.Warnings = append(r.Warnings, "network.cluster_apiserver_address is empty — externally-joined DPUs need a routable apiserver address; without this kubespray's localhost-nginx-proxy hack takes over and DPUs can't reach the apiserver (see AGENTS.md #4)")
	}
	if p.Network.NodeIPRole == "" {
		r.Warnings = append(r.Warnings, "network.node_ip_role is empty — hosts will fall back to ssh.address (mgmt) for kubelet --node-ip, DPUs auto-detect; usually you want this set to the data-plane role (e.g. \"internal\")")
	}

	// --- hosts (presence + per-host shape) ---
	if len(p.Hosts) == 0 {
		r.Errors = append(r.Errors, "no hosts in poc.yaml — run `dpubnkctl discover wizard` (or `discover range`) first")
	}
	cps, workers := 0, 0
	for i, h := range p.Hosts {
		hctx := fmt.Sprintf("hosts[%d:%s]", i, h.Name)
		if h.Name == "" {
			r.Errors = append(r.Errors, fmt.Sprintf("%s.name is empty", hctx))
		}
		switch h.Role {
		case "":
			r.Errors = append(r.Errors, fmt.Sprintf("%s.role is empty (set to control-plane | worker | both)", hctx))
		case "control-plane":
			cps++
		case "worker":
			workers++
		case "both":
			cps++
			workers++
		default:
			r.Errors = append(r.Errors, fmt.Sprintf("%s.role %q invalid (must be control-plane | worker | both)", hctx, h.Role))
		}
		if h.SSH.Address == "" {
			r.Errors = append(r.Errors, fmt.Sprintf("%s.ssh.address is empty", hctx))
		}
		if h.SSH.User == "" {
			r.Errors = append(r.Errors, fmt.Sprintf("%s.ssh.user is empty", hctx))
		}
		if h.SSH.KeyRef == "" {
			r.Errors = append(r.Errors, fmt.Sprintf("%s.ssh.key_ref is empty", hctx))
		} else if !fileExists(repoDir, h.SSH.KeyRef) {
			r.Errors = append(r.Errors, fmt.Sprintf("%s.ssh.key_ref %q file not found (drop the private key into the PoC repo or fix the path)", hctx, h.SSH.KeyRef))
		}

		// Per-DPU.
		for j, d := range h.DPUs {
			dctx := fmt.Sprintf("%s.dpus[%d:%s]", hctx, j, d.PCI)
			validateDPU(&r, &p.Hosts[i].DPUs[j], dctx)
			_ = d
		}

		// Data-plane VLAN sub-interfaces on the host.
		if h.DataPlane != nil {
			if h.DataPlane.ParentIface == "" {
				r.Errors = append(r.Errors, fmt.Sprintf("%s.data_plane.parent_iface is empty (set to the host's data-plane PF, e.g. ens16f0np0)", hctx))
			}
			for k, v := range h.DataPlane.VLANs {
				validateHostVLAN(&r, v, fmt.Sprintf("%s.data_plane.vlans[%d]", hctx, k))
			}
		} else if len(h.DPUs) > 0 {
			r.Warnings = append(r.Warnings, fmt.Sprintf("%s has DPUs but no data_plane block — host won't have a VLAN sub-interface to talk to the fabric (`dpubnkctl host network setup` will skip it)", hctx))
		}
	}
	if len(p.Hosts) > 0 && cps == 0 {
		r.Errors = append(r.Errors, "no control-plane hosts (at least one host needs role: control-plane or both)")
	}
	if len(p.Hosts) > 0 && workers == 0 {
		r.Errors = append(r.Errors, "no worker hosts (at least one host needs role: worker or both)")
	}
	if cps == 2 {
		r.Warnings = append(r.Warnings, "2 control planes is not HA-safe — etcd quorum requires 3 to survive a single failure")
	}

	// --- provisioning ---
	if p.Provisioning.DPUPasswordHashRef == "" {
		r.Errors = append(r.Errors, "provisioning.dpu_password_hash_ref is empty (path to file containing the output of `openssl passwd -1 '<password>'`)")
	} else if !fileExists(repoDir, p.Provisioning.DPUPasswordHashRef) {
		r.Errors = append(r.Errors, fmt.Sprintf("provisioning.dpu_password_hash_ref %q file not found", p.Provisioning.DPUPasswordHashRef))
	}
	if len(p.Provisioning.DPUDNS) == 0 {
		r.Errors = append(r.Errors, "provisioning.dpu_dns is empty (DPU systemd-resolved needs at least one resolver)")
	}
	if len(p.Provisioning.DPUNTP) == 0 {
		r.Errors = append(r.Errors, "provisioning.dpu_ntp is empty (DPU chrony needs at least one source)")
	}

	// --- BNK credentials ---
	if p.BNK.FARKeyRef == "" {
		r.Errors = append(r.Errors, "bnk.far_key_ref is empty (path to the f5-far-auth-key tarball; obtained from F5 license portal)")
	} else if !fileExists(repoDir, p.BNK.FARKeyRef) {
		r.Errors = append(r.Errors, fmt.Sprintf("bnk.far_key_ref %q file not found — drop the FAR tgz into keys/", p.BNK.FARKeyRef))
	}
	if p.BNK.JWTRef == "" {
		r.Errors = append(r.Errors, "bnk.jwt_ref is empty (path to the TEEM JWT; obtained from F5 license portal)")
	} else if !fileExists(repoDir, p.BNK.JWTRef) {
		r.Errors = append(r.Errors, fmt.Sprintf("bnk.jwt_ref %q file not found — drop the JWT into keys/.jwt", p.BNK.JWTRef))
	}
	if p.BNK.ExternalSelfIP == "" {
		r.Warnings = append(r.Warnings, "bnk.external_selfip is empty — required for the F5SPKVlan that TMM binds for north-south traffic; `deploy cne` will fail without it")
	}
	if p.BNK.InternalSelfIP == "" {
		r.Warnings = append(r.Warnings, "bnk.internal_selfip is empty — same shape as external_selfip, internal-side")
	}

	return r
}

// validateDPU runs per-DPU checks. Mirrors the field requirements in
// internal/provision.buildInputs (bf.conf render).
func validateDPU(r *ValidationResult, d *DPU, ctx string) {
	if d.PCI == "" {
		r.Errors = append(r.Errors, fmt.Sprintf("%s.pci is empty", ctx))
	}
	switch d.Mode {
	case "":
		r.Errors = append(r.Errors, fmt.Sprintf("%s.mode is empty (set to dpu | nic)", ctx))
	case "dpu", "nic":
		// ok
	default:
		r.Errors = append(r.Errors, fmt.Sprintf("%s.mode %q invalid (must be dpu | nic)", ctx, d.Mode))
	}
	if d.Hostname == "" {
		r.Errors = append(r.Errors, fmt.Sprintf("%s.hostname is empty (DPU OS hostname, set before flash)", ctx))
	}
	if d.TmfifoIP == "" {
		r.Errors = append(r.Errors, fmt.Sprintf("%s.tmfifo_ip is empty (tmfifo_net0 CIDR, e.g. 192.168.100.2/30)", ctx))
	}
	if len(d.VLANs) == 0 {
		r.Warnings = append(r.Warnings, fmt.Sprintf("%s has no vlans — DPU won't have any data-plane interfaces", ctx))
	}
	for k, v := range d.VLANs {
		vctx := fmt.Sprintf("%s.vlans[%d]", ctx, k)
		validateDPUVLAN(r, v, vctx, d.LAG)
	}
}

func validateDPUVLAN(r *ValidationResult, v DPUVLAN, ctx string, lag bool) {
	if !roleRe.MatchString(v.Role) {
		r.Errors = append(r.Errors, fmt.Sprintf("%s.role %q must match %s (e.g. external, internal, storage)", ctx, v.Role, roleRe.String()))
	}
	if v.Tag <= 0 || v.Tag > 4094 {
		r.Errors = append(r.Errors, fmt.Sprintf("%s.tag %d invalid (must be 1..4094)", ctx, v.Tag))
	}
	if v.IP == "" {
		r.Errors = append(r.Errors, fmt.Sprintf("%s.ip is empty", ctx))
	} else if _, _, err := net.ParseCIDR(v.IP); err != nil {
		r.Errors = append(r.Errors, fmt.Sprintf("%s.ip %q is not a valid CIDR", ctx, v.IP))
	}
	if name := v.PortName(); len(name) > 15 {
		r.Errors = append(r.Errors, fmt.Sprintf("%s derived port name %q exceeds 15 chars (Linux IFNAMSIZ); shorten role", ctx, name))
	}
	if !lag {
		if v.Uplink != "p0" && v.Uplink != "p1" {
			r.Errors = append(r.Errors, fmt.Sprintf("%s.uplink %q invalid (must be p0 or p1 in non-LAG mode)", ctx, v.Uplink))
		}
	}
}

func validateHostVLAN(r *ValidationResult, v HostDataPlaneVLAN, ctx string) {
	if !roleRe.MatchString(v.Role) {
		r.Errors = append(r.Errors, fmt.Sprintf("%s.role %q must match %s", ctx, v.Role, roleRe.String()))
	}
	if v.Tag <= 0 || v.Tag > 4094 {
		r.Errors = append(r.Errors, fmt.Sprintf("%s.tag %d invalid (must be 1..4094)", ctx, v.Tag))
	}
	if v.IP == "" {
		r.Errors = append(r.Errors, fmt.Sprintf("%s.ip is empty", ctx))
	} else if _, _, err := net.ParseCIDR(v.IP); err != nil {
		r.Errors = append(r.Errors, fmt.Sprintf("%s.ip %q is not a valid CIDR", ctx, v.IP))
	}
	if name := v.PortName(); len(name) > 15 {
		r.Errors = append(r.Errors, fmt.Sprintf("%s derived port name %q exceeds 15 chars (Linux IFNAMSIZ); shorten role", ctx, name))
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
