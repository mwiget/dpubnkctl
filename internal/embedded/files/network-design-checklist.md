# Network design checklist

> The pre-sales SE persona walks the customer through this checklist
> **before** running `dpubnkctl provision dpus`. Every answer is recorded
> in `decisions.md` (the rationale, not just the value) and lands in
> `poc.yaml`. Skipping items here is how PoCs get to 80% cluster-up and
> then stall on a network-design surprise.
>
> The agent fills in the "Customer answer" column as it walks the customer
> through. When all rows are answered, the agent calls
> `dpubnkctl validate` to confirm `poc.yaml` matches.
>
> ## Ordering — discovery splits this checklist
>
> Sections **2-6, 8, 9** are **pre-discovery**: the customer can answer
> them from intent (VLAN plan, MTU target, self-IPs, credentials)
> without needing the lab-tech to scan anything.
>
> Sections **1 and 7** are **post-discovery**: they describe per-host
> data (host count, role, data-plane PF interface) that the lab-tech
> populates by running `dpubnkctl discover range <subnet>` against the
> customer's network. The SE then makes educated suggestions from the
> inventory rather than asking the customer to assign every host's role
> manually.
>
> **Walk pre-discovery sections first, then trigger discovery, then
> walk post-discovery sections.** This file lists Section 1 at the top
> only for backwards compatibility with the field — chronologically it
> comes after Section 9.

## 1. Topology (post-discovery — fill after `dpubnkctl discover`)

Once the lab-tech has scanned the customer's subnet, the inventory tells
you which IPs are reachable and how many DPUs (if any) each host has.
Use that to make educated suggestions:

- Hosts **without DPUs** → suggest `role: control-plane` (no data plane
  to host; ideal CP candidates).
- Hosts **with DPUs** → suggest `role: worker` when ≥3 DPU-free hosts
  exist (HA quorum from dedicated CPs), else `role: both` (small lab
  where DPU hosts double as CPs).
- DPUs themselves always join as workers, independent of host role.

State the rationale in plain English when proposing. Customer confirms
or overrides per host.

| # | Question | Why it matters | Lands in `poc.yaml` | Customer answer |
|---|----------|----------------|---------------------|-----------------|
| 1.1 | How many bare-metal hosts? | Drives `expected_hosts`, control-plane count, etcd quorum math. | `topology.expected_hosts`, `hosts[]` |  |
| 1.2 | How many DPUs per host? | Some labs run 1 BF3 per host, some run 2. | `topology.expected_dpus_per_host` |  |
| 1.3 | How many control planes? | 1 = lab convenience, 2 = HA-unsafe (still single point of failure for etcd quorum, see AGENTS.md), 3 = production. | `topology.control_plane_count` + per-host `role` |  |
| 1.4 | Single-node or multi-node? | Affects scheduler tolerations on the control plane and whether DPU workers will run TMM. | `topology.mode` |  |

## 2. DPU uplink design — LAG vs non-LAG

| # | Question | Why it matters | Lands in `poc.yaml` | Customer answer |
|---|----------|----------------|---------------------|-----------------|
| 2.1 | Will the DPU bond its two physical ports (p0+p1) into one LACP LAG, or use them as two independent uplinks? | LAG requires the customer's switch to be configured for LACP (active or passive) on the matching pair of ports with the same VLAN trunk on both. Non-LAG lets you split traffic across two fabrics (e.g. external on p0, internal on p1). | `hosts[*].dpus[*].lag` (true/false) |  |
| 2.2 | If LAG: is the switch side active or passive LACP? | If both sides are passive, the LAG never comes up. Capture the switch-side mode so lab-tech can sanity check at provision time. | `decisions.md` (not directly in poc.yaml) |  |
| 2.3 | If non-LAG: which port (p0 / p1) carries which VLAN role? | Each VLAN sub-interface in `dpu.vlans[]` needs an `uplink: p0` or `uplink: p1`. dpubnkctl rejects non-LAG mode without this. | `hosts[*].dpus[*].vlans[*].uplink` |  |

## 3. VLAN plan

Walk the customer through every VLAN the data plane will use. **At minimum** BNK needs an `external` VLAN (north-south traffic) and an `internal` VLAN (cluster-internal). Some labs add `storage` (for NFS / Ceph) or `replication`.

| # | Question | Why it matters | Lands in `poc.yaml` | Customer answer |
|---|----------|----------------|---------------------|-----------------|
| 3.1 | What VLAN tag is `external`? | OVS port name is derived as `external<tag>` (e.g. `external40`); end-to-end identifiable on both DPU and host. | `network.vlans[]` + per-DPU/host `vlans[].tag` |  |
| 3.2 | What CIDR does `external` use? End-host IPs come from this subnet. | Each DPU and each host data-plane VLAN sub-interface needs an IP from this CIDR. | `network.vlans[external].subnet` + per-host/DPU `vlans[role=external].ip` |  |
| 3.3 | Does `external` have a default gateway (for egress to internet / north-south)? | TMM needs it for return traffic to clients. | `hosts[*].dpus[*].vlans[role=external].default_gateway` |  |
| 3.4 | Same three questions for `internal` VLAN | Cluster-internal pod-to-pod over the fabric. Usually no default gateway. | `network.vlans[internal]` + per-DPU/host vlans |  |
| 3.5 | Any additional roles (`storage`, `replication`, `mgmt`)? | If yes, repeat 3.1-3.3 for each. Role names must match `^[a-z][a-z0-9]{0,9}$` so `<role><tag>` fits the 15-char Linux iface-name limit. | `network.vlans[]` + per-DPU/host vlans |  |

## 4. Pod CIDR + MTU

| # | Question | Why it matters | Lands in `poc.yaml` | Customer answer |
|---|----------|----------------|---------------------|-----------------|
| 4.1 | What pod CIDR? | Must NOT overlap any data-plane VLAN subnet or any mgmt range the customer is using elsewhere. dpubnkctl default `198.18.100.0/24` is from RFC 2544 (benchmarking) and almost always safe — but confirm. | `network.internal_cidr` |  |
| 4.2 | Pod MTU and DPU MTU. | Defaults: 9000 / 8900 (DPU / Pod). Customer's switch fabric must support ≥9000 MTU end-to-end, or these need to drop. Anything <9000 will impact TMM throughput. | `network.dpu_mtu`, `network.pod_mtu` |  |

## 5. Cluster apiserver address + node-IP role

| # | Question | Why it matters | Lands in `poc.yaml` | Customer answer |
|---|----------|----------------|---------------------|-----------------|
| 5.1 | What address (or VIP) do all nodes use to reach the apiserver? | This is what every node writes in kubeconfig. Externally-joined DPUs need it to be a routable data-plane address. AGENTS.md #4 covers why kubespray's default localhost-nginx-proxy hack fails here. | `network.cluster_apiserver_address` |  |
| 5.2 | Which VLAN role provides each node's kubelet `--node-ip`? | Usually the same as 5.1's role (e.g. `internal`). When unset, hosts fall back to `ssh.address` (mgmt) and DPUs auto-detect, which usually picks the wrong interface. | `network.node_ip_role` |  |

## 6. Storage

| # | Question | Why it matters | Lands in `poc.yaml` | Customer answer |
|---|----------|----------------|---------------------|-----------------|
| 6.1 | NFS server + export path? | `local-path-provisioner` is the dpubnkctl default for storage; if the customer has an NFS server they want pods to mount from, capture it. | `network.nfs_server`, `network.nfs_path` |  |

## 7. Per-host data-plane interface

For **each** host:

| # | Question | Why it matters | Lands in `poc.yaml` | Customer answer (per host) |
|---|----------|----------------|---------------------|---------------------------|
| 7.1 | Which physical interface on the host is the data-plane PF (the one the DPU presents)? | netplan adds VLAN sub-interfaces on this PF (e.g. `ens16f0np0`). One PF per host — bonding is the DPU's job, not the host's. | `hosts[*].data_plane.parent_iface` |  |

## 8. BNK self-IPs

| # | Question | Why it matters | Lands in `poc.yaml` | Customer answer |
|---|----------|----------------|---------------------|-----------------|
| 8.1 | What IP should TMM listen on for `external` (client-facing)? | This is the self-IP that `F5SPKVlan` binds. Customer-visible "VIP" lands here. | `bnk.external_selfip` |  |
| 8.2 | What IP for `internal`? | Same shape, internal-side. | `bnk.internal_selfip` |  |

## 9. Pre-flight credentials

These three the customer drops into `keys/` themselves — but the SE confirms they exist before the lab-tech starts:

- [ ] FAR tarball (`keys/f5-far-auth-key.tgz` or wherever `bnk.far_key_ref` points)
- [ ] JWT (`keys/.jwt` or wherever `bnk.jwt_ref` points) — the `jku` URL inside dictates whether the prod or tst FLO values template is used. dpubnkctl detects automatically (see AGENTS.md #15).
- [ ] SSH private key per host (`hosts[*].ssh.key_ref`)

## When this checklist is "done"

1. Every row has a "Customer answer" filled in (or marked N/A with a reason in `decisions.md`).
2. `poc.yaml` reflects the answers.
3. `dpubnkctl validate` passes with no errors. (Warnings about ambiguous defaults are OK if the SE has journaled the decision.)
4. The SE writes a "scope-agreed" entry in `journal/<date>-scope.md` summarising what the customer committed to.

Only after all four is the lab-tech cleared to run `dpubnkctl provision dpus`.
