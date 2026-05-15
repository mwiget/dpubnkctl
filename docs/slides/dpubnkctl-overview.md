---
marp: true
theme: default
paginate: true
size: 16:9
header: 'dpubnkctl · BNK 2.3.0'
footer: 'github.com/mwiget/dpubnkctl'
style: |
  section { font-size: 24px; padding: 40px 56px; line-height: 1.4; }
  h1 { color: #0a3a5c; font-size: 50px; }
  h2 { color: #0a3a5c; font-size: 32px; border-bottom: 2px solid #d1d5db; padding-bottom: 6px; margin-bottom: 12px; min-height: 44px; }
  /* Page counter (Marp's section::after) — make it readable + consistent
     with the gray footer. */
  section::after { font-size: 14px; color: #6b7280; right: 36px; bottom: 18px; }
  h3 { color: #1f2937; font-size: 22px; }
  p { margin: 6px 0; }
  code { font-size: 18px; background: #f1f5f9; padding: 1px 4px; border-radius: 3px; }
  pre { background: #0b1220; color: #e2e8f0; font-size: 16px; padding: 12px 16px; border-radius: 6px; line-height: 1.3; margin: 6px 0; }
  pre code { background: transparent; color: inherit; font-size: inherit; padding: 0; }
  table { font-size: 17px; }
  th { background: #e2e8f0; color: #0a3a5c; }
  blockquote { border-left: 3px solid #0a3a5c; background: #f8fafc; padding: 4px 12px; color: #374151; font-style: normal; margin: 6px 0; }
  .tag { display: inline-block; background: #0a3a5c; color: white; padding: 2px 10px; border-radius: 4px; font-size: 16px; font-weight: 600; letter-spacing: 1px; margin-right: 10px; vertical-align: middle; }
  .quote-user { color: #6b7280; font-family: monospace; font-size: 15px; line-height: 1.3; }
  .quote-agent { color: #0a3a5c; font-family: monospace; font-size: 15px; line-height: 1.3; }
  /* Dense pages: case-study slides + audit table. Smaller body so the
     four-or-five-line reasoning chain + symptom + fix all fit. */
  section.dense { font-size: 19px; padding: 28px 50px; }
  section.dense h2 { font-size: 26px; margin-bottom: 8px; padding-bottom: 4px; }
  section.dense p { margin: 5px 0; }
  section.dense pre { font-size: 13px; padding: 8px 12px; margin: 4px 0; line-height: 1.25; }
  section.dense code { font-size: 15px; }
  section.dense .quote-agent { font-size: 14px; line-height: 1.3; }
  section.dense blockquote { font-size: 15px; padding: 3px 10px; }
  section.dense table { font-size: 14px; }
---

<!-- _class: lead -->
<!-- _paginate: false -->

# dpubnkctl

**F5 BIG-IP Next for Kubernetes —
deploy in one binary, drive with an agent.**

<br>

**Latest:** BNK 2.3.0 · NVIDIA BlueField-3 (DOCA 3.2.0) · k8s 1.30 · kubespray v2.28.1

<small>2.2 maintenance on `release-2.2.0` · 2.3 maintenance on `release-2.3.0` · `main` toward 2.4</small>

<br>

Marcel Wiget · `github.com/mwiget/dpubnkctl`

---

## Why this exists

Manual BNK-on-bare-metal deploy is 20+ steps over ~3.5 hours:

- BFB flash the DPU (mlxconfig, rshim, bf.conf)
- Host VLAN sub-interfaces (netplan)
- Kubespray cluster bring-up
- Externally join DPUs to k8s
- Multus + SR-IOV + NADs
- cert-manager + F5 Lifecycle Operator (FLO)
- CNEInstance + F5SPKVlans + Gateway / HTTPRoute

24+ recurring failure modes catalogued in `AGENTS.md`. Easy to misorder. Painful to redo from a partial state.

> dpubnkctl is the runbook turned into a single Go binary with persistent declarative state.

---

## Two-repo architecture

```
+----------------------+
| dpubnkctl source     |   single binary, BNK-2.3.0-pinned
|  - internal/         |   (DOCA, BFB, FLO, kubespray) -->
|  - embedded/         |   stamped with `git describe`
|    files/AGENTS.md
|    files/personas/
+----------------------+

     | `dpubnkctl init <name>`
     v
+----------------------+
| <customer>-poc repo  |   declarative state - everything
|  - poc.yaml          |   needed to teardown + redeploy
|  - AGENTS.md + personas/  lives here.
|  - keys/ (gitignored)     poc.yaml.status tracks phase progress.
|  - artifacts/, journal/, decisions.md, diagram.txt
+----------------------+
```

The binary is the engine. The PoC repo is the contract.

---

## Two operating modes

### Agentic — primary focus

```
dpubnkctl agent claude   # prints invocation
cd ~/lab/mycustomer && claude
# Inside the session:
"Read AGENTS.md, act as the pre-sales SE persona. Confirm scope with me."
```

The PoC repo ships `AGENTS.md` + three personas (`pre-sales-se`, `lab-tech`, `doc-specialist`) so any agentic CLI — Claude Code, Aider, Gemini, opencode, openai-compat REPL — inherits the same runbook with the same tool allowlists and handoff protocol. **The binary doesn't ship an LLM. You pick the model.**

### Human direct *(work in progress)*

```
dpubnkctl init mycustomer
dpubnkctl discover wizard      # WIP — agentic path is the validated one
dpubnkctl validate
dpubnkctl e2e --yolo
```

Every subcommand is hand-callable; the wizard lags the agentic path and the gap will widen, not narrow.

---

## What `init` creates

```
poc.yaml                  single source of truth - every input to
                          teardown / redeploy lives here

AGENTS.md                 persona-neutral runbook (embedded)
personas/
  pre-sales-se.md         only persona that talks to the customer
  lab-tech.md             DPU / BMC / firmware specialist
  doc-specialist.md       journal + final-report keeper

network-design-checklist.md   SE-fills-in before provisioning
inventory/                discover output (json)
artifacts/                rendered manifests, kubeconfig
journal/                  append-only, one file per phase
decisions.md              SE decision log
keys/                     FAR, JWT, SSH keys (gitignored)
diagram.txt               auto-regenerated topology view
```

---

## `diagram.txt` — homelab, real run

```
K8s cluster: homelab
====================

  +--------------------------+    +--------------------------+
  |         worker1          |    |         worker2          |
  |           both           |    |          worker          |
  |    eth0 192.168.68.66    |    |    eth0 192.168.68.71    |
  +--------------------------+    +--------------------------+
                |                                |
                | kubeadm join                   | kubeadm join
                v                                v
  +--------------------------+    +--------------------------+
  |       worker1-bf3        |    |       worker2-bf3        |
  |        DPU worker        |    |        DPU worker        |
  |          (LAG)           |    |          (LAG)           |
  |  oob_net0 192.168.68.96  |    |  oob_net0 192.168.68.79  |
  +--------------------------+    +--------------------------+

  apiserver: 192.168.50.66:6443  (all 4 node(s) connect here)
```

Auto-refreshed whenever any phase mutates `poc.yaml`.

---

## 8-phase pipeline

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

Symmetric teardown: `destroy bnk → destroy dpus → cluster reset`. Both directions resume-safe via `artifacts/e2e-state.json`.

```
dpubnkctl e2e --yolo                            # ~75 min, resume-safe
dpubnkctl destroy --yolo --confirm-cluster <name>
```

---

## Auto-generated PoC report — exec summary

`dpubnkctl journal report` rolls every phase journal entry + `decisions.md` + `poc.yaml.status` into one markdown handoff. Excerpt from the homelab run:

**~3.5 hours · clean state → working BNK 2.2.0** *(baseline run — see slides 20-22 for the 2.3 update)*

- 2× BlueField-3 DPUs · DOCA 2.9.2
- 4-node Kubernetes 1.32 · single control plane
- F5 Lifecycle Operator v2.9.27-0.2.10 (tst variant, auto-detected from JWT `jku`)
- CNEInstance `Available=True` — 14/14 component conditions Available
- Both `f5-tmm` pods 6/6 Ready, 2/2 readiness gates
- **`HTTP/1.1 200 OK`** through TMM at the external VIP

> Non-linear path (5 destructive consents, 1 scope correction, 4 technical detours), but the phase-gated journal kept every detour traceable, recoverable, and reproducible.

---

## Smoke test — end-to-end traffic

```
$ curl -v http://192.168.40.100/         # external VIP, from worker1 host
< HTTP/1.1 200 OK
< Server: nginx/1.27.5
< Content-Length: 615
<nginx welcome page body>
```

Data path:

```
worker1  ── VLAN 40 trunk ─── DPU TMM 192.168.40.100 listener
                                    │
                                    ▼ Calico pod CIDR 10.233.64.0/18
                                    smoke-nginx pod
```

**Proves:** LAG/LACP trunk · DPU OVS bridges · TMM listener · Calico ↔ TMM integration · BNK GatewayClass + HTTPRoute reconciler.

Every CR + manifest applied during this run is saved verbatim under `artifacts/` — rendered `CNEInstance`, `F5SPKVlan`s, NADs, `Gateway`, etc. — for review, diff against the next deploy, or attaching to a ticket.

---

## Embedded `AGENTS.md` — the runbook

Every `dpubnkctl init` drops a persona-neutral `AGENTS.md` into the PoC repo. Single doc, agentic-CLI-agnostic. Three sections:

- **Source of truth** — `poc.yaml` is the contract. `decisions.md`, `journal/`, `inventory/`, `artifacts/`, `keys/` each have a documented role.
- **YOLO tiers** — read-only (always auto), reversible (auto with `--auto reversible`), destructive (`--yolo` + matching PoC-name confirm). Agents must respect.
- **27 numbered gotchas** — every recurring failure from past PoCs (24 from the 2.2 round + 3 new from the 2.3 migration). One-line symptom / cause / fix. e.g.:

> **#8.** OVS internal ports default to MTU 1500; apiserver TLS handshakes hang from frame loss. Fix: `bf.conf` sets MTU on every internal port.

Agents read `AGENTS.md` first, every session. **New PoCs inherit every lesson the prior PoCs paid for.**

---

## Three personas — separation of duties

Each persona has a **strict tool allowlist** + **NOT-allowed list** + a journal-based handoff protocol. Same constraints regardless of which agentic CLI runs.

- **`pre-sales-se`** — solution architect. *Only* persona that talks to the customer. Owns `decisions.md`. Cannot run destructive commands.
- **`lab-tech`** — DPU / BMC / rshim / mlxconfig / firmware specialist. Runs `discover` + `provision`. Must journal an SE-consent reference before any destructive action. Cannot modify `poc.yaml`.
- **`doc-specialist`** — append-only journal keeper. Writes the day-end summary and final `report.md`. Cannot run infra commands.

The `journal/` directory is the handoff bus: SE consent → lab-tech executes + records → doc-specialist rolls into the report. **Every persona transition is auditable.**

---

## The feedback loop

The PoC repo isn't a one-way deliverable — it's a feedback channel.

```
+----------------------+   journal entries        +----------------------+
| PoC repo             | ────── lessons-learned ──>| dpubnkctl source     |
| (per customer)       |       audit punch list   | repo (engineering)   |
|                      |                          |                      |
| AGENTS.md  <-- embedded/files/AGENTS.md  -------+ AGENTS.md +24 gotchas|
| personas/  <-- embedded/files/personas/  -------+ personas/            |
| code       <-- new pinned binary release -------+ internal/            |
+----------------------+                          +----------------------+
```

Each tagged release embeds the latest hard-won runbook. Lessons surface in **three** ways:

1. **As code** — validate rules, pre-flight checks, readiness gates
2. **As docs** — numbered `AGENTS.md` gotcha (humans + agents read it)
3. **As persona text** — workflow guidance the agent honors per-PoC

The next PoC starts with stronger defaults. Fewer surprises.

---

## Case study — four agent-diagnosed blockers

The next slides show real moments where the agent **caught blockers a flat runbook would not have spotted unaided** — each from a real deploy on BlueField-3 hardware.

Diagnosis **#1** is from the **lake1 dogfood** that built the binary itself, shown across two slides (the hypothesis ruled out by measurement, then the real root cause) so the reasoning-under-uncertainty is visible. The fix lives in the bf.conf template — the homelab PoC three weeks later **never even had to debug it**.

Diagnoses **#2 – #4** are from the **homelab agentic PoC** — first successful end-to-end agent-driven deploy:

- 2 hosts, 1 DPU each, LAG topology · 4-node k8s cluster
- ~3.5 h wall-clock, operator only consented to destructive phases
- `HTTP/1.1 200 OK` from `curl http://192.168.40.100/` through the BNK TMM Gateway to an nginx pod on the worker2 DPU

---

<!-- _class: dense -->

## Agent diagnosis #1 — first hypothesis (ruled out)

**Symptom:** `kubeadm join` from the DPU hung in discovery, timing out repeatedly:

```
GET https://10.10.41.66:6443/api/v1/namespaces/kube-public/configmaps/cluster-info?timeout=10s
→ request canceled while waiting for connection
```

TCP connect ✓. TLS handshake hung. mgmt-path apiserver was instant — broken only on the data-plane VLAN. The agent's first 60 seconds:

<span class="quote-agent">●  TCP up, TLS hangs — smells like a fragmentation / large-packet drop, not auth or routing.</span>
<span class="quote-agent">●  Bisect with `ping -M do`: `size 1472` ✓, `size 4000` ✗. Drops above 1500. PMTU broken somewhere.</span>
<span class="quote-agent">●  **Hypothesis: the host VLAN sub-if's MTU can't exceed its parent's. My netplan sets `internal41` MTU=9000 but doesn't touch `ens16f0np0` — which is probably still at default. Let me confirm.**</span>
<span class="quote-agent">●  `ip link show ens16f0np0` — mtu **9000**. Already raised. Both VLAN children also 9000. **Hypothesis ruled out by the data.**</span>

The parent-vs-VLAN-child mismatch is the textbook MTU bug for this shape — and it was the agent's first guess. The measurement killed it in one step.

> Path MTU is still broken. Something *else* on the path is at 1500. Walk the path →

---

<!-- _class: dense -->

## Agent diagnosis #1 — root cause (the real culprit)

Agent walks the data-plane path end-to-end:

```
host → ens16f0np0 → switch → bond0 → p0/p1 → pf0hpf → br-lag → …
       (9000 ✓)     (9000)   (9000)  (9000)   (?)      (?)
```

<span class="quote-agent">●  `ip link show bond0 p0 p1` — 9000 each.</span>
<span class="quote-agent">●  `ip link show pf0hpf br-lag external40 internal41` (on the DPU) → **mtu 1500**.</span>
<span class="quote-agent">●  **Found it. DPU's OVS internal ports are at default MTU 1500 even though `bond0/p0/p1` are 9000. Host sends 9000-byte frames → enters OVS via pf0hpf MTU 1500 → dropped. The bf.conf template doesn't bump MTU on OVS internal ports.**</span>

**Why it matched the symptom exactly:** TLS Client Hello (~512 B) fit under 1500 and got through. apiserver's Server Hello + Certificate (multi-KB) got silently dropped at `pf0hpf`.

**Fix:**

- Live: `sudo ip link set {br-lag,pf0hpf,external40,internal41} mtu 9000` — worker2 → apiserver instantly dropped to 13 ms; jumbo pings to 8972 ✓.
- Persistent: `bf.conf::ovs-vlan-init.sh` sets MTU on every OVS internal port from `DPUMtu`. **Commit `0815bb0`, AGENTS.md gotcha #8.** Every PoC initialised since ships with the fix.

> Four hours of "the cluster is broken" without an agent. **Feedback loop in action: lesson once, never re-paid.**

---

<!-- _class: dense -->

## Agent diagnosis #2 — ghost mlx5_core PF

**Symptom:** post-BFB flash, `netplan apply` rejects every host VLAN sub-interface with `RTNETLINK answers: No such device`, even though `ip -br a` lists the parent as UP.

The agent's reasoning chain (paraphrased from the live session):

<span class="quote-agent">●  Stale VLAN children from the old tag-41 config? Cleaned up, retried — still nothing.</span>
<span class="quote-agent">●  `networkd-dispatcher reconfigure` — no effect.</span>
<span class="quote-agent">●  `ip link add link ens16f0np0 ... type vlan` directly → `No such device`. Contradicts "UP" in `ip -br a`.</span>
<span class="quote-agent">●  `ethtool -i ens16f0np0` → `Cannot get driver information: No such device`. **Sysfs symlink present but driver gone.**</span>

**Root cause:** BlueField PF detached from kernel post-flash; `mlxfwreset` unsupported in EMBEDDED_CPU mode → only a host reboot recovers (AGENTS.md #9).

**Fix:** `> "reboot both in parallel"` — post-reboot both hosts came up clean.

> A human's first 30 minutes would be in netplan / networkd land. The agent reached `ethtool -i` in step 4.

---

<!-- _class: dense -->

## Agent diagnosis #3 — apiserver-without-VIP

**Symptom:** `dpubnkctl cluster up` exits 1 — kubeadm init hits its 4-minute wait-control-plane timeout. Retries hit "ports already in use" cleanup garbage.

The agent's reasoning chain:

<span class="quote-agent">●  Pulled `/etc/kubernetes/manifests/kube-apiserver.yaml` off worker1.</span>
<span class="quote-agent">●  Saw `--advertise-address=192.168.50.66`. apiserver healthy on `:::6443`.</span>
<span class="quote-agent">●  Cross-referenced `poc.yaml.network.cluster_apiserver_address: 192.168.50.10`.</span>
<span class="quote-agent">●  ".10 is a VIP. With one CP there's no keepalived, no kube-vip — nothing answers at .10. kubeadm's self-healthcheck timed out at 4 min while the apiserver itself was running fine."</span>
<span class="quote-agent">●  "This is exactly AGENTS.md #4: apiserver address must be routable."</span>

**Fix:** `cluster_apiserver_address: 192.168.50.66` (CP's actual internal50 IP), then `cluster reset` + retry. Cleared kubeadm init on the second pass.

The agent journaled the scope correction in `decisions.md` with the rejected alternative (kube-vip) before re-running. That delta later became validate rule #2 in v2.2.0.

---

<!-- _class: dense -->

## Agent diagnosis #4 — SR-IOV SF on wrong driver

**Symptom:** Second `f5-tmm` pod stuck `Pending` — one DPU missing `nvidia.com/bf3_p0_sf1` allocatable, even though both DPUs flashed with the same `PER_PF_NUM_SF=1`.

The agent's reasoning chain:

<span class="quote-agent">●  `kubectl get node ... -o json | jq .status.allocatable` — A/B'd the two DPUs. worker1-bf3 missing the SF resource.</span>
<span class="quote-agent">●  SSH'd to worker1-bf3 via ProxyJump. `devlink port show` — SF port exists but in "inactive/detached" state.</span>
<span class="quote-agent">●  Activated the port. netdev appeared. TMM init container still failed network attachment.</span>
<span class="quote-agent">●  Walked `/sys/bus/auxiliary/devices/mlx5_core.sf.N/driver` symlinks on PF0 vs PF1.</span>
<span class="quote-agent">●  "The manual SF on PF0 is bound to `mlx5_core.sf_cfg` (config-only) instead of `mlx5_core.sf` (full driver that creates the netdev)."</span>

**Fix:** unbind from `sf_cfg`, bind to `sf`, restart sriov-device-plugin, delete stuck TMM pod. Both TMMs went 6/6, CNEInstance flipped `Available=True`.

> Deep enough that no flat runbook covered it. A typical human shop would page a Mellanox SME at step 2. Audit item #9 turned this into a post-flash readiness probe in v2.2.0.

---

## Honest caveats

The agent wasn't infallible. Things it got wrong (and which now show up as v2.2.0 validate rules):

- **Self-inflicted bug #1.** It set `cluster_apiserver_address: 192.168.50.10` at scoping time, optimistic about HA/VIP with no kube-vip plan. The same agent later diagnosed and fixed it (slide 16) — but the cause was its own earlier choice. → **Validate rule #2** now catches this at `dpubnkctl validate` time.

- **Self-inflicted bug #2.** It set `worker2-bf3.tmfifo_ip: 192.168.100.6/30` reasoning "each host needs a unique /30". Wrong — each rshim is a private point-to-point link; the convention is `.2/30` everywhere. Cost: a `cluster join-dpus` retry. → **Validate rule #4** now flags non-`.2/30` values.

- **Tried to bypass its own guardrail.** It invoked `--skip-validate` on the destructive BFB flash after writing in `decisions.md` that skip-validate "is never appropriate". The auto-mode classifier blocked it. The agent self-corrected: *"I hit my own guardrail."*

These all closed in v2.2.0. Future PoCs start with stronger defaults — see next slide.

---

<!-- _class: dense -->

## Audit closeout — v2.2.0 round

| #  | Item                                              | Resolution kind   |
|----|---------------------------------------------------|-------------------|
| 1  | validate phase-blind (FAR/JWT blocking provision) | refactor          |
| 2  | cluster_apiserver_address not cross-checked       | new validate rule |
| 3  | network.vlans[] silently dropped role/tag         | strict yaml load  |
| 4  | tmfifo_ip didn't catch wrong /30                  | new validate rule |
| 5  | cluster join-dpus not idempotent                  | code fix          |
| 6  | kubelet stayed dead on join failure               | code fix          |
| 7  | deploy cne raced FLO crd-installer                | two-step wait     |
| 8  | deploy network returned mid-DS-flap               | rollout-status    |
| 9  | post-flash SF aux device race                     | readiness probe   |
| 10 | kubespray "ip var" error opaque                   | pre-flight        |
| 11 | ghost mlx5_core PF needed reboot                  | pre-flight        |
| 12 | doc-specialist promised non-existent --pdf        | persona fix       |
| 14 | no Gateway scaffolding (BNK 2.2.0 has no IPAM)    | new subcommand    |
| 15 | provision exit-0-on-timeout misleading            | grace + hard fail |

All in `main`. Each item carries a journal-entry reference in its commit message — future engineers can read the failure narrative.

---

## BNK 2.2 → 2.3 — three shape changes

The 2.3 release wasn't an incremental bump. Three real changes that
break the 2.2 deploy shape:

| What changed | 2.2.0 | 2.3.0 |
|---|---|---|
| **License location** | JWT + TEEM URLs + x5c chain inside `flo-values.yaml` (separate prod/tst templates) | `License` CR (`k8s.f5net.com/v1`) in `f5-cne-core` namespace; CWC auto-detects prod/tst from JWT `jku` |
| **Chart versions** | FLO chart pinned in `version.go` (`v2.9.27-0.2.10`) | Pulled at deploy time from F5's `f5-bigip-k8s-manifest` release-manifest chart (`2.3.0-3.2598.3-0.0.170`) |
| **CWC TLS material** | Implicit in FLO chart | Operator runs `f5-cert-gen` helm chart + applies two Secrets before CWC starts |

The prod/tst auto-detection is the cleanest. Drop a tst JWT in
`keys/.jwt` and `kubectl get license` shows `ENVIRONMENT=test`
without any per-environment configuration — no separate template,
no flag, no override. The CWC just knows.

DOCA bumped from 2.9.2 → 3.2.0 (Ubuntu 22.04 → 24.04, kernel 5.15 →
6.8). Same bf.conf, same OVS port list — kernel 6.8 ships dual SF
interface names but the legacy `en3f0pf0sf1` still works.

---

<!-- _class: dense -->

## 2.3 migration — 8 new audit items

Same agentic loop as the 2.2.0 round (slides 13-17). The 2.3 e2e
on homelab found 8 new gotchas; all closed in the `feat/bnk-2.3.0`
branch before the v2.3.0 tag.

| # | Item | Where caught |
|---|------|--------------|
| 25 | DOCA 3.2 BFB pre-ships `kubernetes.sources` (v1.34) — apt resolves the higher version, kubeadm refuses 1.30 cluster | `cluster join-dpus` first run |
| — | K8s version `1.30.14` typed in `poc.yaml` makes apt URL `v1.30.14/deb` → silently 1.34 | Same |
| — | `gen_cert.sh` emits 1-space-indented YAML; YAML injection mangled it | `deploy flo` step 10 |
| — | kubectl 1.30 lacks `--for=create` (added in 1.31) | `deploy cne` step 3 |
| — | License `Registering` state not in switch; default 5min wait too short | `deploy cne` step 6 |
| — | Multus first-start race on worker1-bf3 (loopback-only delegate) | Post-deploy smoke |
| 26 | License auto-detects prod vs tst from JWT `jku` — Phase 3 hypothesis | Verified live |
| 27 | BNK 2.3 HTTPRoutes require `hostnames:` (Gateway-API conformance) — without it, TMM returns BigIP 500 | Smoke `curl` |

Each surfaced live, was diagnosed, fixed, captured in a Conventional
Commit + AGENTS.md gotcha. The branch carries 13 commits total: 4
features, 9 fixes. Every fix is reusable for the next BNK release.

---

<!-- _class: dense -->

## 2.3.0 deploy confirmed — homelab, real run

Same hardware as slide 7; same `dpubnkctl e2e --yolo --no-resume`.

**`HTTP/1.1 200 OK` through TMM — clean state → 2.3 deploy**

- 2× BlueField-3 DPUs · **DOCA 3.2.0** · Ubuntu 24.04 · kernel 6.8
- 4-node Kubernetes **1.30.14** · single control plane
- F5 Lifecycle Operator **v2.21.13-0.0.28** (resolved at deploy time from `f5-bigip-k8s-manifest` 2.3.0-3.2598.3-0.0.170)
- License CR **`STATE=Active`** `ENVIRONMENT=test` `MODE=connected`
  ↑ tst-vs-prod auto-detected from JWT `jku` — zero operator config
- CNEInstance `Available=True` — all component conditions met
- Both `f5-tmm` pods 6/6 Ready, 2/2 readiness gates
- `Gateway demo-gw` `Programmed=True` @ `192.168.40.100`
- `destroy --yolo` round-trip clean: kubespray reset.yml 0 failures, License + CWC + observer finalizers stripped, **zero "unknown flag" noise**

> The agentic loop that closed the 2.2.0 round (slides 13-19) ran
> again for 2.3 — 8 fresh gotchas surfaced live, each diagnosed →
> committed → AGENTS.md gotcha in a single session. Same pattern,
> next BNK release.

---

## Where next

- `release-2.3.0` cut, `release-2.2.0` remains the 2.2.x maintenance home
- `main` proceeds toward BNK 2.4 (no public ETA from F5 yet)
- Multi-DPU-per-host (host-side tmfifo netplan + relaxed validate)
- Live TMM self-IP capture from F5SPKVlan after deploy
- IPAM auto-allocation if BNK adds a default-pool concept
- Generalised pre-sales SE workflow for non-BNK F5 products

The binary is `~16 MB`, statically linked, single-file. Drop it on a
jumphost and you're one `dpubnkctl init` away from a reproducible
PoC.

```
go install github.com/mwiget/dpubnkctl/cmd/dpubnkctl@v2.3.0
```

<br>

**Questions / feedback:** `github.com/mwiget/dpubnkctl/issues`
