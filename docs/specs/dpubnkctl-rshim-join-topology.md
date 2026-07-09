# dpubnkctl rshim-join topology (DPU joins over tmfifo + host-NAT internet)

Status: implemented (Pattern 1 + tmfifo allocation). Multi-host routing deferred.
Date: 2026-07-08.

## Problem

`cluster join-dpus` historically joined each DPU over a **data-plane VLAN**
IP. On a fresh bare-metal deploy that VLAN can't carry the control plane yet:
the host-PF↔SF e-switch steering that bridges the host↔DPU internal VLAN only
exists **after** BNK deploy/SF-CNI runs — a chicken-and-egg that leaves the DPU
unreachable at join time (DPU `pf1hpf` RX=0).

The working D-020 SSH blueprint avoids this entirely: the DPU joins over the
**rshim/tmfifo** link (host-local, always present after flash) and gets internet
via **host NAT**. The data VLAN is reserved for TMM/CNE traffic set up later.
This topology mirrors D-020 in dpubnkctl.

Two patterns:

1. **single-host-rshim** — 1 host (control-plane + worker), 1 DPU over tmfifo. **Shipped.**
2. **multi-host-rshim** — N hosts, 1 DPU each, unique tmfifo addresses from a
   pool. Addressing shipped; cross-host **routing** deferred (see below).

## Selector

`network.join_transport: rshim | vlan` (default `vlan` = the original behavior,
so existing PoCs are unchanged). A flag, not a new `topology.mode` value — this
avoids a mode×transport combinatorial explosion. `rshim` selects everything below.

## Key implementation note: kubespray, not raw kubeadm

D-020 runs raw `kubeadm init --apiserver-cert-extra-sans`. **dpubnkctl does not** —
`cluster up` renders a **kubespray inventory** and runs kubespray in Docker. So the
"apiserver advertises on the host tmfifo IP + extra SANs" decision maps onto
kubespray group_vars, not kubeadm flags. Only the **DPU-side** join is real kubeadm
(`cluster.JoinDPU`), where the `--node-ip` change lands directly.

## Decisions

### node-ip
`resolveDPUNodeIP` returns the DPU's bare `tmfifo_ip` when `join_transport=rshim`
(the DPU's default route is tmfifo, so its kubelet InternalIP lands there). The
host keeps its mgmt (`ssh.address`) node-ip — unchanged cluster identity.

### apiserver address + cert SANs
- The DPU joins against the **control-plane host's tmfifo IP** (`192.168.100.1`
  single-host). `FetchJoinCommand` rewrites the kubeadm join line to this address.
- The host keeps its mgmt node-ip and advertise address; **no** kubespray
  `loadbalancer_apiserver` override for rshim.
- `renderGroupVarsAll` adds each control-plane host's **mgmt IP + tmfifo IP** to
  `supplementary_addresses_in_ssl_keys`, so both the DPU (tmfifo path) and the
  operator's kubectl (mgmt path) verify TLS.
- Rejected alternative: apiserver on the host mgmt IP + a DPU `ip route
  <host_ip>/32 via <tmfifo>` host-route. Extra moving part, no benefit single-host.

### setup-dpu-networking (host NAT + DPU route/DNS)
Ports D-020 `setup_dpu_networking.py::_execute_tmfifo`:
- **host**: `sysctl net.ipv4.ip_forward=1` + idempotent `iptables -t nat -A
  POSTROUTING -s <tmfifo_subnet> -o <default_iface> -j MASQUERADE`.
- **DPU**: `ip route replace default via <host_tmfifo_ip>`, `/etc/resolv.conf`
  from `provisioning.dpu_dns`, verify `ping -c1 8.8.8.8`.
- **No** DPU host-route to the apiserver: it's advertised on the tmfifo IP the
  DPU already reaches directly (single-host).

**Where it runs (Q1 — chosen: in-join pre-step; alternatives documented so a
future pivot needs no re-research):**

| Option | Behavior | Trade-off |
|--------|----------|-----------|
| **A — in-join pre-step (CHOSEN)** | Runs at the top of `joinOneDPU`, before `InstallKubeBinaries`. | Automatic; exactly mirrors D-020 order (net-setup → install → join). The apt install always has internet. Coupled to the join command. |
| B — standalone command | e.g. `dpubnkctl cluster setup-dpu-networking`, run between provision and join. | Inspectable in isolation; more operator/agent steps. To pivot: lift `setupDPUNetworking` into a new cobra command, drop the call from `joinOneDPU`. |
| C — end of `provision dpu` | Append after second-boot reachability. | Runs early, but the apiserver isn't up at provision time (fine for NAT/DNS; a host-route would need deferring). |

Why A: dpubnkctl installs kubelet/kubeadm/kubectl **inside** `cluster join-dpus`
(not a separate phase), and that apt install needs internet — so networking must
precede it in the same command. `setupDPUNetworking` in
`internal/cli/setup_dpu_networking.go` is a standalone function, so moving to B is
a small refactor.

### Persistence — systemd oneshot units
A warm reboot happens during provisioning, so both sides are made persistent via
self-contained idempotent systemd oneshot units (`enable --now` applies now +
arms for boot):
- host: `dpubnkctl-tmfifo-nat.service` (+ `/usr/local/sbin/dpubnkctl-tmfifo-nat.sh`)
- DPU: `dpubnkctl-tmfifo-route.service` (+ `/usr/local/sbin/dpubnkctl-tmfifo-route.sh`)

`destroy dpus` tears both down (best-effort) under rshim. Chosen over
iptables-persistent + netplan to avoid an extra package dependency and keep
teardown fully in our control.

### tmfifo addressing + allocation
Each host↔DPU tmfifo link is a separate point-to-point /30 (host `.1`, DPU `.2`).
- **single-host, no `tmfifo_cidr`**: the rshim driver default 192.168.100.1/.2.
- **`network.tmfifo_cidr` set** (e.g. `192.168.0.0/24`): `AllocateTmfifo` carves a
  **unique /30 per DPU** from the pool (deterministic by host/dpu index),
  persisting `host.tmfifo_ip` + `dpu.tmfifo_ip` back to poc.yaml for idempotent
  redeploy. This fixes the historic "dup tmfifo" collision at its root and makes
  each kubelet node-ip cluster-unique. `ensureHostTmfifoIPCIDR` also removes the
  stale rshim default when a pool address is in use.

Allocation runs at `provision dpu` (persisted, before bf.conf render) and again at
`cluster join-dpus` (idempotent safety net). `provision plan` allocates in-memory
only (read-only command).

## Multi-host routing — deferred (Pattern 2, step 5)

Unique addressing alone doesn't make host1's control plane reach host2's DPU: each
tmfifo segment is host-local. Options to evaluate next (prototype on 2 hosts):
- **(A) Routed mesh** — each host routes other DPUs' tmfifo /32s via the owning
  host's mgmt IP + forwards; Calico no-overlay/BGP or IP-in-IP.
- **(B) Calico overlay (VXLAN/IPIP)** — pod traffic tunnelled host-to-host over the
  mgmt underlay; node-to-node *control* (apiserver → kubelet at the other DPU's
  tmfifo IP) still needs (A)'s routed reachability. Likely (A)+(B).

Confirm what D-020 multi-host does today before inventing. Single-host has none of
this.

## poc.yaml fields

| Field | Meaning |
|-------|---------|
| `network.join_transport` | `rshim` \| `vlan` (default). Selects the topology. |
| `network.tmfifo_cidr` | Multi-host rshim pool, e.g. `192.168.0.0/24`. Empty ⇒ single-host default /30. |
| `host.tmfifo_ip` | Host-side tmfifo CIDR. Allocated + persisted; empty ⇒ 192.168.100.1/30. |
| `provisioning.dpu_internet` | `host-nat` \| `oob` \| `none`. Empty ⇒ host-nat under rshim, none otherwise. |

## Verify (Pattern 1)

DPU in DPU mode (warm reboot, no cold cycle) → `cluster up` (apiserver reachable
over tmfifo) → `cluster join-dpus`: DPU `Ready`, node InternalIP = tmfifo IP, DPU
has internet (`ping 8.8.8.8`, apt works) → `deploy` proceeds to a licensed FLO/CNE.

## Source map

- `internal/poc/schema.go` — `JoinTransport`, `TmfifoCIDR`, `Host.TmfifoIP`,
  `DPUInternet`, consts, `EffectiveDPUInternet`.
- `internal/poc/tmfifo.go` — `AllocateTmfifo`.
- `internal/poc/validate.go` — rshim-aware validation.
- `internal/cli/cluster_join_dpus.go` — `resolveDPUNodeIP`, apiserver address,
  `dpuNetSetup`, `ensureTmfifoAllocated`, `bareIP`.
- `internal/cli/setup_dpu_networking.go` — the host-NAT + DPU-route step + units.
- `internal/cli/provision_dpu.go` — `ensureHostTmfifoIPCIDR/For`, allocation call.
- `internal/cluster/inventory.go` — rshim `supplementary_addresses_in_ssl_keys`.
- `internal/cli/destroy.go` — unit teardown.
- `examples/single-node-rshim.yaml` — worked example.
