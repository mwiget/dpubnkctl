---
marp: true
theme: default
paginate: true
size: 16:9
header: 'dpubnkctl · BNK 2.2.0'
footer: 'github.com/mwiget/dpubnkctl'
style: |
  section { font-size: 24px; padding: 50px 60px; }
  h1 { color: #0a3a5c; font-size: 50px; }
  h2 { color: #0a3a5c; font-size: 34px; border-bottom: 2px solid #d1d5db; padding-bottom: 6px; }
  h3 { color: #1f2937; font-size: 22px; }
  code { font-size: 18px; background: #f1f5f9; padding: 1px 4px; border-radius: 3px; }
  pre { background: #0b1220; color: #e2e8f0; font-size: 16px; padding: 14px 18px; border-radius: 6px; line-height: 1.35; }
  pre code { background: transparent; color: inherit; font-size: inherit; padding: 0; }
  table { font-size: 17px; }
  th { background: #e2e8f0; color: #0a3a5c; }
  blockquote { border-left: 3px solid #0a3a5c; background: #f8fafc; padding: 6px 14px; color: #374151; font-style: normal; }
  .tag { display: inline-block; background: #0a3a5c; color: white; padding: 2px 10px; border-radius: 4px; font-size: 16px; font-weight: 600; letter-spacing: 1px; margin-right: 10px; vertical-align: middle; }
  .quote-user { color: #6b7280; font-family: monospace; font-size: 16px; }
  .quote-agent { color: #0a3a5c; font-family: monospace; font-size: 16px; }
---

<!-- _class: lead -->
<!-- _paginate: false -->

# dpubnkctl

**F5 BIG-IP Next for Kubernetes —
deploy in one binary, drive with an agent.**

<br>

BNK 2.2.0 · NVIDIA BlueField-3 · k8s 1.32 · kubespray v2.28.1

<br>

Marcel Wiget · `github.com/mwiget/dpubnkctl`

---

## <span class="tag">1</span>Why this exists

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

## <span class="tag">2</span>Two-repo architecture

```
+----------------------+
| dpubnkctl source     |   single binary, BNK-2.2.0-pinned
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

## <span class="tag">3</span>Two operating modes

### Human direct

```
dpubnkctl init mycustomer
dpubnkctl discover wizard
dpubnkctl validate
dpubnkctl e2e --yolo
```

### Agentic

```
dpubnkctl agent claude   # prints invocation
cd ~/lab/mycustomer && claude
# Inside the session:
"Read AGENTS.md, act as the pre-sales SE persona. Confirm scope with me."
```

The PoC repo ships `AGENTS.md` + three personas — `pre-sales-se`, `lab-tech`, `doc-specialist` — so any agentic CLI (Claude Code, Aider, Gemini, opencode, openai-compat REPL) inherits the same runbook with the same tool allowlists and handoff protocol. **The binary doesn't ship an LLM. You pick the model.**

---

## <span class="tag">4</span>What `init` creates

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

## <span class="tag">5</span>`diagram.txt` — homelab, real run

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

## <span class="tag">6</span>8-phase pipeline

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

## <span class="tag">7</span>The feedback loop

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

## <span class="tag">8</span>Case study — four agent-diagnosed blockers

The next four slides show real moments where the agent **caught blockers a flat runbook would not have spotted unaided** — each from a real deploy on BlueField-3 hardware.

The first (slide 9) is from the **lake1 dogfood** that built the binary itself; the fix lives in the bf.conf template, so the homelab PoC three weeks later **never even had to debug it**.

The other three (slides 10-12) are from the **homelab agentic PoC** — first successful end-to-end agent-driven deploy:

- 2 hosts, 1 DPU each, LAG topology · 4-node k8s cluster
- ~3.5 h wall-clock, operator only consented to destructive phases
- `HTTP/1.1 200 OK` from `curl http://192.168.40.100/` through the BNK TMM Gateway to an nginx pod on the worker2 DPU

---

## <span class="tag">9</span>Agent diagnosis #1 — apiserver hang from OVS-port MTU

**Symptom:** `kubeadm join` from the DPU hung in discovery, timing out repeatedly:

```
GET https://10.10.41.66:6443/api/v1/namespaces/kube-public/configmaps/cluster-info?timeout=10s
→ request canceled while waiting for connection
```

TCP connect succeeded. TLS handshake never completed. The agent's reasoning:

<span class="quote-agent">●  TCP up, TLS hangs — smells like a fragmentation / large-packet drop, not auth or routing.</span>
<span class="quote-agent">●  Bisect MTU on the data-plane VLAN: `ping -M do -s 1472` ✓, `ping -M do -s 4000` ✗. Drops above 1500.</span>
<span class="quote-agent">●  bond0 / p0 / p1 are 9000 (set by netplan from bf.conf). What's at 1500?</span>
<span class="quote-agent">●  Walked the path host → pf0hpf → br-lag → bond0 → fabric. `ip link show pf0hpf` → **mtu 1500**. OVS-internal ports default to 1500 even when the bridge's uplinks are 9000.</span>

**Root cause:** TLS Client Hello (~512 B) fit under 1500 and got through. The apiserver's Server Hello + Certificate (multi-KB) got dropped at the OVS internal port. kubeadm's per-request 10s timeout retried forever, never seeing the response.

**Fix:** `bf.conf::ovs-vlan-init.sh` now sets MTU on `br-lag`, `pf0hpf`, `pf1hpf`, and every per-VLAN OVS port (commit `0815bb0`, AGENTS.md #8). The fix lives in the binary's embedded template — every PoC initialised since then ships with it.

> Four hours of "the cluster is broken" without an agent. **The feedback loop in action: lesson once, never re-paid.**

---

## <span class="tag">10</span>Agent diagnosis #2 — ghost mlx5_core PF

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

## <span class="tag">11</span>Agent diagnosis #3 — apiserver-without-VIP

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

## <span class="tag">12</span>Agent diagnosis #4 — SR-IOV SF on wrong driver

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

## <span class="tag">13</span>Honest caveats

The agent wasn't infallible. Things it got wrong (and which now show up as v2.2.0 validate rules):

- **Self-inflicted bug #1.** It set `cluster_apiserver_address: 192.168.50.10` at scoping time, optimistic about HA/VIP with no kube-vip plan. The same agent later diagnosed and fixed it (slide 10) — but the cause was its own earlier choice. → **Validate rule #2** now catches this at `dpubnkctl validate` time.

- **Self-inflicted bug #2.** It set `worker2-bf3.tmfifo_ip: 192.168.100.6/30` reasoning "each host needs a unique /30". Wrong — each rshim is a private point-to-point link; the convention is `.2/30` everywhere. Cost: a `cluster join-dpus` retry. → **Validate rule #4** now flags non-`.2/30` values.

- **Tried to bypass its own guardrail.** It invoked `--skip-validate` on the destructive BFB flash after writing in `decisions.md` that skip-validate "is never appropriate". The auto-mode classifier blocked it. The agent self-corrected: *"I hit my own guardrail."*

These all closed in v2.2.0. Future PoCs start with stronger defaults — see next slide.

---

## <span class="tag">14</span>Audit closeout — v2.2.0 round

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

## <span class="tag">15</span>Where next

- Cut `v2.2.0` branch from current main as BNK 2.3.0 work begins
- More PoCs feed more audit items
- Multi-DPU-per-host (host-side tmfifo netplan + relaxed validate)
- Live TMM self-IP capture from F5SPKVlan after deploy
- IPAM auto-allocation if BNK exposes a default-pool concept
- Generalised pre-sales SE workflow for non-BNK F5 products

The binary is `~16 MB`, statically linked, single-file. Drop it on a jumphost and you're one `dpubnkctl init` away from a reproducible PoC.

```
go install github.com/mwiget/dpubnkctl/cmd/dpubnkctl@latest
```

<br>

**Questions / feedback:** `github.com/mwiget/dpubnkctl/issues`
