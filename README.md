# dpubnkctl

![Go](https://img.shields.io/github/go-mod/go-version/mwiget/dpubnkctl)
![License](https://img.shields.io/github/license/mwiget/dpubnkctl)
![Last commit](https://img.shields.io/github/last-commit/mwiget/dpubnkctl)
[![Release](https://img.shields.io/github/v/release/mwiget/dpubnkctl?label=download)](https://github.com/mwiget/dpubnkctl/releases/latest)

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
4. **cluster** — bring up Kubernetes via kubespray (Calico CNI), then
   join DPU workers externally via kubeadm
5. **deploy** — install BNK platform: Multus + SR-IOV + NADs +
   local-path-provisioner (`deploy network`), cert-manager + FLO
   (with FAR image-pull secret + bnk-ca cert chain, `deploy flo`),
   CNEInstance + F5SPKVlan self-IPs + License CR + GatewayClass
   (`deploy cne`)

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

Pinned to BNK 2.3.0 / Kubernetes 1.30.14 / DOCA 3.2.0 / release manifest 2.3.0-3.2598.3-0.0.170 (FLO chart resolved at deploy time). A different BNK release ships a different `dpubnkctl` build.

## Two operating modes

Pick the one that matches your environment — both go end-to-end without
any post-wizard hand-edits when the lab follows the conventions in
`examples/`.

**Non-agentic** — `dpubnkctl wizard` walks you through subnet + SSH
credentials + per-host role + customer + 2 VLAN tag/subnet pairs, then
fills the rest of `poc.yaml` from documented conventions (per-host VLAN
IPs from mgmt last-octet, per-DPU sequential IPs, self-IPs at `.100`,
cluster apiserver address from first CP host, etc.). 6 operator answers
in a typical lab; everything else accepts the default. See **Quick start
(non-agentic)** below.

**Agentic (BYO CLI)** — `dpubnkctl init` drops `AGENTS.md` and three
persona files (`personas/pre-sales-se.md`, `personas/lab-tech.md`,
`personas/doc-specialist.md`) into the PoC repo. Point Claude Code,
Gemini, Aider, an OpenAI-compatible REPL, [pi](https://pi.dev/), or
[opencode](https://opencode.ai/) at it via `dpubnkctl agent <cli>`. The
agent reads AGENTS.md and conversationally walks the phases, suggests
defaults from lab notes, and records *why* each decision was made in
`decisions.md`. See **Quick start (agentic)** below.

The binary doesn't ship an LLM — you choose the model and endpoint
(cloud or local vLLM) appropriate for your customer's compliance posture.

## Versioning

This binary is **per-BNK-release.** Building from `main` today targets
**BNK 2.3.0** with these pins:

| Component | Version |
|-----------|---------|
| DOCA / BFB | 3.2.0 (`bf-bundle-3.2.0-113_25.10_ubuntu-24.04_64k_prod.bfb`) |
| F5 Lifecycle Operator chart | resolved at deploy time from release manifest |
| Release manifest | 2.3.0-3.2598.3-0.0.170 |
| cert-manager | v1.16.2 |
| Kubernetes | 1.30.14 (kubespray v2.28.1) |
| containerd | 1.7.23 |
| runc | 1.2.1 |
| pause image | 3.10 |
| DPU MTU / Pod MTU | 9000 / 8900 |

A different BNK release ships a different `dpubnkctl` build. Do not mix.
**Maintenance fixes for BNK 2.2.0 land on the `release-2.2.0` branch.**

### 2.2 → 2.3 in one sentence

Licensing moved out of FLO chart values into a `License` CR
(`k8s.f5net.com/v1`); the F5 Cluster-Wide Controller (CWC) reads the
TEEM endpoint from the JWT's `jku` header so operators no longer split
prod-vs-tst at the FLO layer. FLO + CIS + cert-gen chart versions
are resolved at deploy time from F5's `f5-bigip-k8s-manifest` chart
rather than pinned in this binary. See `MIGRATING-2.3.0.md`.

## Optional: bnk-forge integration

[bnk-forge](https://github.com/sp-prod-field/bnk-forge) (separate
project, currently private) is F5's Day-2 UI for BNK. dpubnkctl can
opt into it via a `bnk_forge:` block in `poc.yaml`:

```yaml
bnk_forge:
    enabled: true
    repo_path: ~/git/bnk-forge
    url: https://localhost
```

With that block enabled, `cluster up` auto-registers the cluster
(uploads the localized kubeconfig) with a project named after
`poc.metadata.name`. The cluster + BNK come into view in the
bnk-forge UI as soon as kubespray finishes — useful for watching the
rest of the pipeline (FLO, CWC, License, TMM) come up live.

dpubnkctl **never installs bnk-forge for you**. If the local stack
isn't running, the auto-hook logs a clean skip and the deployment
continues; bring it up manually with `make deploy` in the bnk-forge
clone and run `dpubnkctl bnk-forge launch` to register after the
fact.

See AGENTS.md gotcha #29 for the full failure-mode rundown.

## Self-contained binary

`dpubnkctl` is a single static Go binary. Ship just the binary — nothing
else. All assets the operator and any agentic CLI need at deploy time are
embedded inside it via `go:embed`:

- `AGENTS.md`, `CLAUDE.md`, and the three persona files dropped into the
  PoC repo on `dpubnkctl init`
- `bf.conf` templates (LAG + non-LAG) for the BFB flash
- FLO Helm values, License CR template, CNEInstance, F5SPKVlan, NAD manifests
- Pinned component versions (BFB image, FLO chart, kubespray, k8s,
  containerd, runc, pause)

The two things customers supply separately are credentials, delivered
through F5's normal channels — never via this binary:

- `keys/<name>.tgz` — FAR image-pull key (from F5 license portal)
- `keys/.jwt`        — TEEM JWT (from F5 license portal)
- `keys/<name>`      — operator SSH private key for the lab hosts

Drop those into `keys/` of the PoC repo `init` creates. Everything else
is in the binary.

## Download

Prebuilt binaries for each tagged release are on the
[**GitHub Releases page**](https://github.com/mwiget/dpubnkctl/releases/latest) —
three archives per release plus a `checksums.txt`:

| Platform | Archive |
|---|---|
| Linux (Intel/AMD) | `dpubnkctl_<version>_linux_amd64.tar.gz` |
| Linux (ARM64) | `dpubnkctl_<version>_linux_arm64.tar.gz` |
| macOS (Apple Silicon) | `dpubnkctl_<version>_darwin_arm64.tar.gz` |

One-liner install (Linux amd64; swap the suffix for your platform):

```bash
VERSION=$(curl -fsSL https://api.github.com/repos/mwiget/dpubnkctl/releases/latest | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p')
curl -fsSL "https://github.com/mwiget/dpubnkctl/releases/download/${VERSION}/dpubnkctl_${VERSION#v}_linux_amd64.tar.gz" \
  | tar -xz -C /tmp dpubnkctl
sudo install -m 0755 /tmp/dpubnkctl /usr/local/bin/dpubnkctl
dpubnkctl version
```

Releases follow `v<bnk-version>-<n>` — e.g. `v2.3.0-1`, `v2.3.0-2`.
The `2.3.0` prefix tracks the pinned BNK release; the `-n` suffix
increments per dpubnkctl-only iteration.

Or build from source — see [Build](#build) below.

## Requirements

On the operator workstation (where you run `dpubnkctl`):

| Tool / resource | Why | Notes |
|---|---|---|
| **Docker Engine** *or* **Podman** | Runs the pinned `kubespray:v2.28.1` and `alpine/k8s:1.31.5` containers — used by `cluster up`, `cluster reset`, `cluster join-dpus`, `deploy *`, `destroy *`. Docker is tried first, then podman. | Daemon (docker) / binary (podman) must respond to `version`. Install: https://docs.docker.com/engine/install/ or https://podman.io/docs/installation |
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

## Quick start (non-agentic)

The wizard-driven workflow. Six operator answers (subnet/range, SSH user,
SSH key, role per discovered host, customer name) plus five Enter-presses
for the network-design defaults gets you a deployable PoC. Tested against
the homelab in `wizard-verify-log.txt`.

```bash
# 1. Create a fresh PoC repo. Creates ./customer-x/ with:
#      poc.yaml, AGENTS.md, CLAUDE.md, personas/, journal/,
#      inventory/, artifacts/, keys/, decisions.md, .gitignore
#    Also auto-generates a random DPU OS password into
#      keys/dpu_password.{hash,txt}
dpubnkctl init customer-x --customer "Customer X"
cd customer-x

# 2. Drop the three operator-supplied files into keys/
cp /path/to/your-ssh-private-key        keys/id_ed25519
cp /path/to/f5-far-auth-key.tgz         keys/f5-far-auth-key.tgz
cp /path/to/license.jwt                 keys/.jwt

# 3. Run the wizard. Answers needed (defaults in brackets are usually fine):
#      - subnet/range to scan          e.g. 192.168.68.0/24
#      - SSH user                      [ubuntu]
#      - SSH port                      [22]
#      - SSH key path                  keys/id_ed25519
#      - jumphost                      [blank]
#      - role per discovered host      both | control-plane | worker
#      - customer name                 [from --customer]
#      - external VLAN tag/subnet      [40 / 192.168.40.0/24]
#      - internal VLAN tag/subnet      [50 / 192.168.50.0/24]
#      - node_ip_role                  [internal]
#    Writes everything to poc.yaml plus inventory/<host>/discover.json
#    per reachable host.
dpubnkctl wizard

# 4. Confirm the PoC validates clean.
dpubnkctl validate

# 5. Run the full pipeline (60–90 min on real hardware).
#    Per-phase logs land in reports/<timestamp>/logs/NN-<phase>.log.
#    Resume-safe via artifacts/e2e-state.json (re-run e2e --yolo after
#    a transient failure and it picks up where it left off).
dpubnkctl e2e --yolo

# 6. Tear down (symmetric — bnk → dpus → cluster reset):
dpubnkctl destroy --yolo --confirm-cluster customer-x
```

That's it. Every poc.yaml field the wizard doesn't ask is filled from
documented conventions matching the shipped examples; review/tweak
`poc.yaml` between step 3 and step 4 if your environment differs.

## Quick start (agentic)

The conversational workflow. The agent reads `AGENTS.md`, walks you
through scope, populates `poc.yaml`, and records *why* each decision
was made.

```bash
dpubnkctl init customer-x --customer "Customer X"
cd customer-x

# Drop the three operator-supplied files into keys/ (same as non-agentic):
cp /path/to/your-ssh-private-key        keys/id_ed25519
cp /path/to/f5-far-auth-key.tgz         keys/f5-far-auth-key.tgz
cp /path/to/license.jwt                 keys/.jwt

# Pick your agentic CLI; the binary prints the right invocation:
dpubnkctl agent claude                  # Claude Code
dpubnkctl agent gemini                  # Gemini
dpubnkctl agent aider                   # Aider
dpubnkctl agent pi                      # pi (https://pi.dev/)
dpubnkctl agent opencode                # opencode (https://opencode.ai/)
dpubnkctl agent openai                  # any OpenAI-compatible REPL (set --llm-endpoint)

# Then inside the agent session, e.g.:
#   "Read AGENTS.md, act as the pre-sales SE persona. Confirm scope with me."
```

## Per-phase invocation (advanced)

Both workflows above end with `dpubnkctl e2e --yolo` which runs every
phase. If you'd rather drive phases one at a time (for diagnostics,
partial reruns, or curriculum-style demos), the canonical order matches
what `dpubnkctl e2e --help` prints:

```bash
dpubnkctl validate
dpubnkctl provision dpu <hosts> --yolo --confirm-flash <hosts>
dpubnkctl host network setup --yolo --confirm-cluster <name>
dpubnkctl cluster up --yolo --confirm-cluster <name>
dpubnkctl cluster join-dpus --yolo --confirm-cluster <name>
dpubnkctl deploy network --yolo --confirm-deploy <name>
dpubnkctl deploy flo --yolo --confirm-deploy <name>
dpubnkctl deploy cne --yolo --confirm-deploy <name>
```

`<hosts>` is the comma-separated hostname list for `--confirm-flash`
*and* the positional argument list for `provision dpu` — both must
match (typo guard). `<name>` is `poc.yaml.metadata.name`. Every phase
is idempotent and gated by `--yolo` plus its `--confirm-*` flag.

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

- BFB image (`bf-bundle-3.2.0-...bfb`) — downloaded once, cached in
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
