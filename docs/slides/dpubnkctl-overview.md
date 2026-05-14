# dpubnkctl — Overview deck

11-slide brief on what dpubnkctl is, who runs it, and why it gets
smarter with every PoC.

Source of truth: this markdown. The companion `dpubnkctl-overview.html`
renders the same content with print-friendly CSS — open in any browser
and Print → Save as PDF. Or convert via pandoc:

```
pandoc dpubnkctl-overview.md \
  -o dpubnkctl-overview.pdf \
  --pdf-engine=xelatex -V geometry:margin=1in
```

---

## 1 · dpubnkctl

**F5 BIG-IP Next for Kubernetes — deploy in one binary,
drive with an agent.**

Targets BNK 2.2.0 · NVIDIA BlueField-3 · Kubernetes 1.32 · kubespray v2.28.1

Marcel Wiget · https://github.com/mwiget/dpubnkctl

---

## 2 · Why this exists

Manual BNK-on-bare-metal deploy is 20+ steps over ~3.5 hours:

- BFB flash the DPU (mlxconfig, rshim, bf.conf)
- Host VLAN sub-interfaces (netplan)
- Kubespray cluster bring-up
- Externally join DPUs to k8s
- Multus + SR-IOV + NADs
- cert-manager + F5 Lifecycle Operator
- CNEInstance + F5SPKVlans + Gateway/HTTPRoute

24+ recurring failure modes catalogued in AGENTS.md.
Easy to misorder. Painful to redo from a partial state.

**dpubnkctl is the runbook turned into a single Go binary
with a persistent declarative state.**

---

## 3 · Two-repo architecture

```
+----------------------+
| dpubnkctl source     |          single binary, BNK-2.2.0-pinned
|  - internal/         |   ──►   (DOCA, BFB, FLO, kubespray)
|  - embedded/         |          stamped with `git describe`
|    files/AGENTS.md
|    files/personas/
+----------------------+

     | `dpubnkctl init <name>`
     v
+----------------------+
| <customer>-poc repo  |          declarative state — everything
|  - poc.yaml          |   ──►   needed to teardown + redeploy
|  - AGENTS.md + personas/      lives here.
|  - keys/ (gitignored)         poc.yaml.status tracks phase progress.
|  - artifacts/, journal/,
|    decisions.md, diagram.txt
+----------------------+
```

The binary is the engine. The PoC repo is the contract.

---

## 4 · Two operating modes

**Human direct:**

```
dpubnkctl init mycustomer
dpubnkctl discover wizard
dpubnkctl validate
dpubnkctl e2e --yolo
```

**Agentic:**

```
dpubnkctl agent claude   # prints invocation
cd ~/lab/mycustomer && claude

# Inside the session:
"Read AGENTS.md, act as the pre-sales SE persona.
 Confirm scope with me."
```

The PoC repo ships `AGENTS.md` + three personas — `pre-sales-se`,
`lab-tech`, `doc-specialist` — so any agentic CLI (Claude Code, Aider,
Gemini, opencode, openai-compat REPL) inherits the same runbook with
the same tool allowlists and handoff protocol.

The binary doesn't ship an LLM. You pick the model.

---

## 5 · What `init` creates

Files inside the PoC repo — also where the agent lives:

```
poc.yaml                 single source of truth — every input
                         to teardown/redeploy lives here

AGENTS.md                persona-neutral runbook (embedded)
personas/
  pre-sales-se.md        only persona that talks to the customer
  lab-tech.md            DPU/BMC/firmware specialist
  doc-specialist.md      journal + final-report keeper

network-design-checklist.md   SE-fills-in before provisioning
inventory/               discover output (json)
artifacts/               rendered manifests, kubeconfig
journal/                 append-only, one file per phase
decisions.md             SE decision log
keys/                    FAR, JWT, SSH keys (gitignored)
diagram.txt              auto-regenerated topology view
```

---

## 6 · diagram.txt — homelab, real run

```
K8s cluster: homelab
====================

  +--------------------------+    +--------------------------+
  |         worker1          |    |         worker2          |
  |           both           |    |          worker          |
  |    eth0 192.168.68.66    |    |    eth0 192.168.68.71    |
  +--------------------------+    +--------------------------+
              |                               |
              | kubeadm join                  | kubeadm join
              v                               v
  +--------------------------+    +--------------------------+
  |       worker1-bf3        |    |       worker2-bf3        |
  |        DPU worker        |    |        DPU worker        |
  |          (LAG)           |    |          (LAG)           |
  |  oob_net0 192.168.68.96  |    |  oob_net0 192.168.68.79  |
  +--------------------------+    +--------------------------+

  apiserver: 192.168.50.66:6443  (all 4 node(s) connect here)

Mgmt-plane (out-of-band SSH addresses)
======================================
  Node           Role      Iface       Address
  worker1        both      eth0        192.168.68.66
  worker1-bf3    dpu       oob_net0    192.168.68.96
  worker2        worker    eth0        192.168.68.71
  worker2-bf3    dpu       oob_net0    192.168.68.79

  external VLAN 40 (external40) — 192.168.40.0/24
    worker1        192.168.40.66
    worker1-bf3    192.168.40.5
    worker2        192.168.40.71
    worker2-bf3    192.168.40.6
```

Auto-refreshed whenever any phase mutates `poc.yaml`.

---

## 7 · 8-phase pipeline

```
1. validate            poc.yaml schema sanity + phase-tagged rules
2. provision dpu       BFB flash, mlxconfig SR-IOV, bf.conf, wait
3. host network setup  netplan VLAN sub-interfaces
4. cluster up          kubespray cluster.yml
5. cluster join-dpus   externally join BlueField nodes to k8s
6. deploy network      Multus + SR-IOV CNI + NADs
7. deploy flo          cert-manager + F5 Lifecycle Operator
8. deploy cne          CNEInstance + F5SPKVlans + BNK GatewayClass
```

Symmetric teardown: `destroy bnk → destroy dpus → cluster reset`.
Both directions resume-safe via `artifacts/e2e-state.json`.

The one-shot:

```
dpubnkctl e2e --yolo                  # ~75 min, resume-safe
dpubnkctl destroy --yolo --confirm-cluster <name>
```

---

## 8 · The feedback loop

The PoC repo isn't a one-way deliverable — it's a feedback channel.

```
+----------------------+    journal entries     +----------------------+
| PoC repo             | ──── lessons-learned ──>| dpubnkctl source     |
| (per customer)       |    audit punch list    | repo (engineering)   |
|                      |                        |                      |
| AGENTS.md ◄──── embedded/files/AGENTS.md ─────┤ AGENTS.md +24 gotchas|
| personas/ ◄──── embedded/files/personas/ ─────┤ personas/            |
| code ◄──── new pinned binary release ─────────┤ internal/            |
+----------------------+                        +----------------------+
```

Each tagged release embeds the latest hard-won runbook. Gotchas surface
in three ways depending on what's actionable:

1. **As code** — validate rules, pre-flight checks, readiness gates
2. **As docs** — numbered AGENTS.md gotcha (humans + agents read it)
3. **As persona text** — workflow guidance the agent honors per-PoC

The next PoC starts with stronger defaults. Fewer surprises.

---

## 9 · Case study: homelab agentic PoC

First successful end-to-end agentic deploy on real BlueField-3 hardware.

- 2 hosts, 1 DPU each, LAG topology
- 4-node Kubernetes cluster (2 hosts + 2 DPUs)
- ~3.5 hours wall-clock — agent-driven, operator only consented to
  destructive phases
- `HTTP/1.1 200 OK` from `curl http://192.168.40.100/` through the BNK
  TMM Gateway to an nginx pod on the worker2 DPU

Journals + decisions.md captured 15 distinct pain points the agent or
operator had to work around. Each became an audit item in the v2.2.0
hardening round.

---

## 10 · Audit closeout — v2.2.0 round

| # | Item                                              | Resolution kind   |
|---|---------------------------------------------------|-------------------|
| 1 | validate phase-blind (FAR/JWT blocking provision) | refactor          |
| 2 | cluster_apiserver_address not cross-checked       | new validate rule |
| 3 | network.vlans[] silently dropped role/tag         | strict yaml load  |
| 4 | tmfifo_ip didn't catch wrong /30                  | new validate rule |
| 5 | cluster join-dpus not idempotent                  | code fix          |
| 6 | kubelet stayed dead on join failure               | code fix          |
| 7 | deploy cne raced FLO crd-installer                | two-step wait     |
| 8 | deploy network returned mid-DS-flap               | rollout-status    |
| 9 | post-flash SF aux device race                     | readiness probe   |
|10 | kubespray "ip var" error opaque                   | pre-flight        |
|11 | ghost mlx5_core PF needed reboot                  | pre-flight        |
|12 | doc-specialist promised non-existent --pdf        | persona fix       |
|13 | (rendered moot by item 6)                         | —                 |
|14 | no Gateway scaffolding (BNK 2.2.0 has no IPAM)    | new subcommand    |
|15 | provision exit-0-on-timeout misleading            | grace + hard fail |

All in `main`. Each item carries a journal-entry reference in its
commit message — future engineers can read the failure narrative.

---

## 11 · Where next

- Cut `v2.2.0` branch from current main as BNK 2.3.0 work begins
- More PoCs feed more audit items
- Multi-DPU-per-host (host-side tmfifo netplan + relaxed validate)
- Live TMM self-IP capture from F5SPKVlan after deploy
- IPAM auto-allocation if BNK exposes a default-pool concept
- Generalised pre-sales SE workflow for non-BNK F5 products

The binary is `~16 MB`, statically linked, single-file. Drop it on a
jumphost and you're one `dpubnkctl init` away from a reproducible PoC.

```
go install github.com/mwiget/dpubnkctl/cmd/dpubnkctl@latest
```

— end —
