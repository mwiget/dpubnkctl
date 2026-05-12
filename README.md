# dpubnkctl

Single-binary CLI to deploy F5 BIG-IP Next for Kubernetes (BNK) on bare-metal
hosts with NVIDIA BlueField DPUs.

> **Status: Phase 0 (skeleton).** `init`, `agent`, and `version` work today.
> `discover`, `provision`, `cluster`, `deploy`, `destroy`, and `journal` are
> stubs that report which phase will implement them.

## What this tool does

Drives a PoC from raw hardware to a working BNK deployment in four phases:

1. **discover** — probe hosts and DPUs (SSH + Redfish + rshim/mlxconfig),
   classify, build inventory
2. **provision** — flash DPUs with the pinned BFB image, set NIC mode,
   configure VLAN/LAG networking
3. **cluster** — bring up Kubernetes (kubespray for ≥3 hosts, kubeadm
   otherwise), join DPU workers, install CNI/Multus/SR-IOV/storage
4. **deploy** — install BNK platform: cert-manager, FLO, CNEInstance,
   VLAN self-IPs, GatewayClass

Each PoC lives in its own local git repo so you can tear down and redeploy
from `poc.yaml` alone.

## Two operating modes

**Human:** run subcommands directly. `dpubnkctl --help`, `dpubnkctl init
demo`, then walk the phases.

**Agentic (BYO CLI):** `dpubnkctl init` drops `AGENTS.md` and three persona
files (`personas/pre-sales-se.md`, `personas/lab-tech.md`,
`personas/doc-specialist.md`) into the PoC repo. Point Claude Code, Gemini,
Aider, or any OpenAI-compatible REPL at it via `dpubnkctl agent <cli>`.
The binary doesn't ship an LLM — you choose the model and endpoint
(cloud or local vLLM) appropriate for your customer's compliance posture.

## Versioning

This binary is **per-BNK-release.** Building from `main` today targets
**BNK 2.2.0** with these pins:

| Component | Version |
|-----------|---------|
| DOCA / BFB | 2.9.2 (`bf-bundle-2.9.2-32_25.02_ubuntu-22.04_prod.bfb`) |
| F5 Lifecycle Operator chart | v2.9.27-0.2.10 |
| Kubernetes | 1.29 |
| containerd | 1.7.23 |
| runc | 1.2.1 |
| pause image | 3.10 |
| DPU MTU / Pod MTU | 9000 / 8900 |

A different BNK release ships a different `dpubnkctl` build. Do not mix.

## Build

```bash
make build               # → bin/dpubnkctl
make smoke               # build + minimal end-to-end smoke (init + agent)
make install             # → ~/.local/bin/dpubnkctl
```

## Quick start (Phase 0)

```bash
dpubnkctl init customer-x
cd customer-x
$EDITOR poc.yaml         # set topology, expected_hosts, network plan
dpubnkctl agent claude   # prints the Claude Code invocation
```

## Repo layout (the binary itself)

```
cmd/dpubnkctl/           main entrypoint
internal/cli/            cobra commands (root, init, agent, version, stubs)
internal/poc/            poc.yaml schema + I/O
internal/version/        build-stamped version + pinned BNK component versions
internal/embedded/       go:embed of AGENTS.md, personas/, .gitignore template
```

## Repo layout (a PoC created by `dpubnkctl init`)

```
poc.yaml                 declarative state — source of truth
AGENTS.md                instructions for any agentic CLI
CLAUDE.md                @AGENTS.md include
personas/                pre-sales-se | lab-tech | doc-specialist
journal/                 append-only markdown log
inventory/               populated by `dpubnkctl discover`
artifacts/               bf.conf renders, kubeconfig, helm values
keys/                    gitignored — FAR tgz, JWT, SSH keys
decisions.md             running decision log
.gitignore               excludes secret material
```

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
