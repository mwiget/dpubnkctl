# dpubnkctl

Single-binary CLI to deploy F5 BIG-IP Next for Kubernetes (BNK) on bare-metal
hosts with NVIDIA BlueField DPUs.

**📊 [Overview deck](https://mwiget.github.io/dpubnkctl/)** — the why,
the human + agentic operating modes, the homelab case study (with four
agent-diagnosed blockers), and the v2.2.0 audit closeout. Best first
read.

Recurring failure modes are documented in [AGENTS.md](AGENTS.md) — read
it before driving a new lab. `journal list/add/report`, `validate`,
`doctor`, and `topologies` are wired.

## What this tool does

Drives a PoC from raw hardware to a working BNK deployment in five phases:

1. **discover** — probe hosts and DPUs (SSH + Redfish + rshim/mlxconfig),
   classify, build inventory. Three modes: `discover host <ip>`,
   `discover range <subnet>`, or `discover wizard` (interactive prompts)
2. **provision** — flash DPUs with the pinned BFB image, set NIC mode,
   configure VLAN/LAG networking
3. **host network setup** — netplan VLAN sub-interfaces on each host
   for the data-plane fabric (e.g. `external40`, `internal41`)
4. **cluster** — bring up Kubernetes via kubespray, join DPU workers
   externally, install CNI/Multus/SR-IOV/storage
5. **deploy** — install BNK platform: cert-manager, FLO (with FAR
   image-pull secret + bnk-ca cert chain), CNEInstance, F5SPKVlan
   self-IPs

Symmetric **`destroy`** tears the same stack down — `bnk` (workload
+ FLO + cert-manager) → `dpus` (kubeadm reset over SSH) → `cluster`
(kubespray reset.yml) — with stuck-finalizer cleanup baked in.

Each PoC lives in its own local git repo so you can tear down and redeploy
from `poc.yaml` alone.

## Supported topologies

| Topology | Hosts | Control planes | Workers | HA-safe? | Use case |
|---|---|---|---|---|---|
| Single-node lab | 1 | 1 (role: both) | 1 (role: both) | No | Smoke test, demo on a laptop |
| Minimum multi-node | 2 | 1 | 1 host + DPUs | No | First customer PoC |
| Two control planes | 2 | 2 (role: both) | DPUs (and CPs) | No\* | Common lab PoC |
| Production HA | 3+ | 3 | DPUs + opt. hosts | Yes | Long-running deployments |

\* 2 control planes keeps etcd quorum on a single failure but a 3rd host is needed for true HA. `dpubnkctl validate` warns on this.

Per topology:

- **DPUs always join externally** (not via kubespray). Each host has 1+ DPUs declared in `poc.yaml.hosts[*].dpus`. `dpubnkctl cluster join-dpus` joins them after the kubespray run.
- **Each host can be control-plane, worker, or both** via `host.role`. "Both" is the typical lab shape — control-plane scheduling is enabled on those nodes.
- **Each DPU runs LAG or non-LAG.** LAG bonds p0+p1 into one fabric uplink (single VLAN trunk on the switch side); non-LAG uses p0 + p1 as two independent uplinks with `vlans[].uplink: p0|p1`.
- **No fixed upper bound on hosts/DPUs.** The schema and pipeline accept arbitrary host + DPU counts; specific shapes have been exercised against bare-metal labs of the topologies listed above.

Pinned to BNK 2.2.0 / Kubernetes 1.32.8 / DOCA 2.9.2 / FLO v2.9.27-0.2.10. A different BNK release ships a different `dpubnkctl` build.

## Two operating modes

**Human:** run subcommands directly. `dpubnkctl --help`,
`dpubnkctl init demo`, then walk the phases. For first-time onboarding
without prior context, `dpubnkctl discover wizard` walks you through
subnet + SSH credentials + per-host role assignment.

**Agentic (BYO CLI):** `dpubnkctl init` drops `AGENTS.md` and three
persona files (`personas/pre-sales-se.md`, `personas/lab-tech.md`,
`personas/doc-specialist.md`) into the PoC repo. Point Claude Code,
Gemini, Aider, an OpenAI-compatible REPL, [pi](https://pi.dev/), or
[opencode](https://opencode.ai/) at it via `dpubnkctl agent <cli>`.
The agent reads AGENTS.md and conversationally walks the phases —
the discover wizard equivalent comes for free, with much better
context awareness (suggests sensible defaults from lab notes, records
*why* each decision was made in `decisions.md`).

The binary doesn't ship an LLM — you choose the model and endpoint
(cloud or local vLLM) appropriate for your customer's compliance posture.

## Versioning

This binary is **per-BNK-release.** Building from `main` today targets
**BNK 2.2.0** with these pins:

| Component | Version |
|-----------|---------|
| DOCA / BFB | 2.9.2 (`bf-bundle-2.9.2-32_25.02_ubuntu-22.04_prod.bfb`) |
| F5 Lifecycle Operator chart | v2.9.27-0.2.10 |
| cert-manager | v1.16.2 |
| Kubernetes | 1.32.8 (kubespray v2.28.1) |
| containerd | 1.7.23 |
| runc | 1.2.1 |
| pause image | 3.10 |
| DPU MTU / Pod MTU | 9000 / 8900 |
| CNE manifest | 2.2.0-3.2226.0-0.0.385 |

A different BNK release ships a different `dpubnkctl` build. Do not mix.

## Self-contained binary

`dpubnkctl` is a single static Go binary. Ship just the binary — nothing
else. All assets the operator and any agentic CLI need at deploy time are
embedded inside it via `go:embed`:

- `AGENTS.md`, `CLAUDE.md`, and the three persona files dropped into the
  PoC repo on `dpubnkctl init`
- `bf.conf` templates (LAG + non-LAG) for the BFB flash
- FLO Helm values (prod + tst), CNEInstance, F5SPKVlan, NAD manifests
- Pinned component versions (BFB image, FLO chart, kubespray, k8s,
  containerd, runc, pause)

The two things customers supply separately are credentials, delivered
through F5's normal channels — never via this binary:

- `keys/<name>.tgz` — FAR image-pull key (from F5 license portal)
- `keys/.jwt`        — TEEM JWT (from F5 license portal)
- `keys/<name>`      — operator SSH private key for the lab hosts

Drop those into `keys/` of the PoC repo `init` creates. Everything else
is in the binary.

## Requirements

On the operator workstation (where you run `dpubnkctl`):

| Tool / resource | Why | Notes |
|---|---|---|
| **Docker Engine** *or* **Podman** | Runs the pinned `kubespray:v2.28.1` and `alpine/k8s:1.32.8` containers — used by `cluster up`, `cluster reset`, `cluster join-dpus`, `deploy *`, `destroy *`. Docker is tried first, then podman. | Daemon (docker) / binary (podman) must respond to `version`. Install: https://docs.docker.com/engine/install/ or https://podman.io/docs/installation |
| **git** | `dpubnkctl init` git-inits the PoC repo (skippable with `--no-git`) | Any recent version |
| **Mgmt network** with outbound to: `content.mellanox.com`, `github.com`, `quay.io`, `repo.f5.com`, `registry-1.docker.io` | BFB image, cert-manager manifests, kubespray + alpine/k8s images, FLO Helm chart and BNK container images | Pulled at runtime; not embedded. The mgmt network typically has internet; the data-plane usually doesn't (AGENTS.md #23). |
| **SSH access** to every host and DPU BMC | All probes, flash, and config are SSH/Redfish-driven; no `ssh` binary needed (Go stdlib) | Operator's private key referenced from `keys/` in the PoC repo |

Verify everything with one command after install:

```bash
dpubnkctl doctor              # host tools + network reachability + (if poc.yaml) keys
dpubnkctl doctor --strict     # also fail on warnings (CI / unattended use)
dpubnkctl doctor --skip-network  # offline-friendly
```

You do **not** need to install kubectl, helm, ansible, python, rshim, or
mlxconfig on the operator workstation. Cluster-side tools run inside
containers; DPU-side tools live on the DPU OS after the BFB flash.

What customers supply themselves, dropped into `keys/` of the PoC repo
(delivered through F5's normal channels — not via this binary):

- FAR tarball — image-pull credentials for `repo.f5.com`
- JWT — TEEM activation token
- SSH private key for the lab hosts

## Example PoC shapes

The `examples/` directory carries pre-canned `poc.yaml` skeletons for
the four supported topologies (single-node, two-host-2cp, two-host-cp-worker,
three-host-ha), using RFC-2544 / RFC-5737 placeholder addresses so
they're safe to commit. Crib one as a starting point:

```bash
dpubnkctl init customer-x
cp examples/two-host-2cp.yaml customer-x/poc.yaml
$EDITOR customer-x/poc.yaml          # replace placeholders with lab-real values
dpubnkctl validate --poc customer-x  # confirm consistency before provisioning
```

See `examples/README.md` for the placeholder address ranges and the
host/IP conventions each example follows.

## Build

```bash
make build               # → bin/dpubnkctl
make smoke               # build + minimal end-to-end smoke (init + agent)
make install             # → ~/.local/bin/dpubnkctl
```

## Quick start

```bash
# 1. Create a PoC repo with binary defaults + persona files + AGENTS.md.
dpubnkctl init customer-x
cd customer-x

# 2a. Drop SSH key + FAR tarball + JWT into keys/ (gitignored).
cp ~/.ssh/id_ed25519 keys/customer-x
cp /downloads/f5-far-auth-key.tgz keys/
cp /downloads/customer.jwt keys/.jwt

# 2b. Either edit poc.yaml by hand OR auto-discover:
dpubnkctl discover wizard          # interactive — prompts for subnet, etc.
# or:
dpubnkctl discover range 192.168.68.0/24 --ssh-user ubuntu --ssh-key keys/customer-x

# 2c. Walk network-design-checklist.md with the customer (LAG vs non-LAG,
#     VLAN tags + IP subnets, MTU, cluster_apiserver_address, self-IPs).
#     Record answers in poc.yaml, rationale in decisions.md.
$EDITOR network-design-checklist.md poc.yaml decisions.md

# 2d. Confirm poc.yaml is internally consistent + all referenced files exist.
dpubnkctl validate                 # errors + warnings; non-zero exit on errors

# 3. Phase through the deploy. All commands are idempotent + gated by
#    --yolo + --confirm-cluster <name> for the destructive ones.
#    `provision dpus` re-runs `validate` as a precheck.
dpubnkctl provision dpus           # flash BFB, configure networking
dpubnkctl host network setup       # data-plane VLAN sub-ifs on hosts
dpubnkctl cluster up               # kubespray cluster.yml (~30 min)
dpubnkctl cluster join-dpus        # DPUs join with --node-ip on data-plane
dpubnkctl deploy network           # Multus + SR-IOV + NADs + local-path
dpubnkctl deploy flo               # cert-manager + FLO + bnk-ca + far-secret
dpubnkctl deploy cne               # CNEInstance + F5SPKVlans

# 4. Tear down (same shape):
dpubnkctl destroy                  # bnk → dpus → cluster reset

# Or drive the whole thing conversationally:
dpubnkctl agent claude             # prints Claude Code invocation
dpubnkctl agent pi                 # prints pi (https://pi.dev/) invocation
dpubnkctl agent opencode           # prints opencode invocation
```

## Repo layout (the binary itself)

```
cmd/dpubnkctl/           main entrypoint
internal/cli/            cobra commands (init, discover, provision,
                         cluster, deploy, destroy, agent, version)
internal/poc/            poc.yaml schema + I/O
internal/cluster/        kubespray inventory + Docker runner + DPU join
internal/deploy/         Helm/kubectl runner + JWT/FAR parsing
internal/provision/      bf.conf rendering for BFB flash
internal/discover/       SSH probes + range scan + result classification
internal/ssh/            ProxyJump-aware SSH client
internal/embedded/       go:embed of AGENTS.md, personas/, bf.conf templates,
                         CNEInstance + FLO values + NAD manifests
internal/version/        build-stamped + BNK component pins
AGENTS.md                23 recurring failure modes + style for coding agents
```

## Repo layout (a PoC created by `dpubnkctl init`)

```
poc.yaml                          declarative state — source of truth
AGENTS.md                         instructions for any agentic CLI
CLAUDE.md                         @AGENTS.md include
network-design-checklist.md       SE-customer scope worksheet (LAG, VLANs, IPs, MTU)
personas/                         pre-sales-se | lab-tech | doc-specialist
journal/                          append-only markdown log (auto-appended each phase)
inventory/                        populated by `dpubnkctl discover`
artifacts/                        bf.conf renders, kubeconfig, helm values, manifests
keys/                             gitignored — FAR tgz, JWT, SSH keys
decisions.md                      running decision log
.gitignore                        excludes secret material
```

## What's embedded (and what isn't)

For operators or auditors who want the full inventory of what ships
inside the binary. Two `go:embed` trees live in `internal/embedded/`:

**`files/` — copied verbatim into a PoC repo on `dpubnkctl init`:**

```
AGENTS.md                       PoC-operations guide for agentic CLIs
CLAUDE.md                       one-liner: @AGENTS.md
poc.gitignore                   excludes keys/, *.tgz, .jwt, kubeconfig
personas/pre-sales-se.md        solution architect — owns scope + decisions.md
personas/lab-tech.md            DPU/BMC/rshim/mlxconfig specialist
personas/doc-specialist.md      journal keeper + final report
```

**`templates/` — rendered at runtime, never written to the PoC repo:**

```
bf-lag.conf.tmpl                BFB flash config, LAG mode
bf-nolag.conf.tmpl              BFB flash config, single-uplink mode
flo-values.yaml.tmpl            FLO Helm values — PRD (product.apis.f5.com
                                URLs + PRD RSA modulus + x5c chain)
flo-values-tst.yaml.tmpl        FLO Helm values — TST (product-tst.apis.
                                f5networks.net + TST RSA modulus + x5c)
cne-instance.yaml.tmpl          CNEInstance manifest
f5spkvlan.yaml.tmpl             F5SPKVlan self-IP manifest
bnk-gatewayclass.yaml.tmpl      GatewayClass for BNK
network/multus.yaml             Multus CNI daemonset
network/sriovdp-daemonset.yaml  SR-IOV device plugin
network/sriovdp-config.yaml     SR-IOV resource pools
network/sriov-cni-daemonset.yaml  SR-IOV CNI binary deployer
network/cni-plugins.yaml        CNI plugin installer
network/local-path-provisioner.yaml  default storage class
network/nad-sf.yaml             NetworkAttachmentDefinition for DPU SFs
```

The FLO classifier picks `flo-values.yaml.tmpl` vs `flo-values-tst.yaml.tmpl`
from the JWT's `jku` URL — TST tokens are signed against a different JWKS
and need the matching RSA modulus baked into the template.

**Not embedded — fetched at runtime over the mgmt network:**

- BFB image (`bf-bundle-2.9.2-...bfb`) — downloaded once, cached in
  `~/.cache/dpubnkctl/bfb/`. Default URL is pinned but overridable in
  `poc.yaml.provisioning.bfb_url`.
- `cert-manager.yaml` from `github.com/cert-manager/cert-manager`
  releases at the pinned version.
- F5 Lifecycle Operator Helm chart from `oci://repo.f5.com/charts/` —
  authenticated with the customer's FAR credentials.
- kubespray — run inside an `alpine/k8s` Docker container at the pinned
  ref; not vendored.

**Customer-supplied (delivered through F5's normal channels, dropped
into `keys/` of the PoC repo):**

- FAR tarball (image-pull credentials for `repo.f5.com`)
- JWT (TEEM activation token)
- SSH private key for the lab hosts

## Design references

- [`roksbnkctl`](https://github.com/jgruberf5/roksbnkctl) — sister tool for
  BNK-on-ROKS; structural inspiration (cobra, embedded assets, persona
  pattern).
- [`f5-bnk-nvidia-bf3-installations`](https://github.com/sp-prod-field/f5-bnk-nvidia-bf3-installations)
  branch `v2.2.0-static` — the kubespray + ansible reference deployment
  this binary automates.
- `bnk-forge` — the multi-version community toolbox. `dpubnkctl` is
  product-managed and BNK-version-pinned; `bnk-forge` is community and
  spans versions.
