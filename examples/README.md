# Example PoC shapes

Pre-canned `poc.yaml` skeletons matching the topologies in
`dpubnkctl topologies`. Embedded in the binary — list with
`dpubnkctl samples`, extract with `dpubnkctl samples extract <name>`
or seed a fresh PoC with `dpubnkctl init <poc-name> --sample <name>`.

The TEST-NET shapes use RFC 5737 placeholder addresses (safe to
commit, obvious to spot when un-edited in a real lab). The
`two-node-homelab` and `single-node-jumphost` shapes use realistic
private ranges (`192.168.10.x`, `10.40.0.x`, `10.50.0.x`) because
they're derived from working labs — every `# CUSTOMIZE:` comment
marks exactly the fields the operator must change.

To use one as a starting point:

```bash
# One step (recommended):
dpubnkctl init customer-x --sample two-host-2cp --customer "Customer X"
cd customer-x
$EDITOR poc.yaml                    # edit CUSTOMIZE lines
dpubnkctl validate

# Two steps:
dpubnkctl init customer-x
cd customer-x
dpubnkctl samples extract two-host-2cp --force   # overwrites the stub poc.yaml
$EDITOR poc.yaml
dpubnkctl validate
```

Or run the wizard against the lab's mgmt subnet to populate hosts from
discovery, then crib the rest of the example:

```bash
cd customer-x
dpubnkctl discover wizard           # populates hosts[] from real probes
# then copy network/topology/bnk blocks from the matching example
```

## What each example covers

| File | Hosts | CPs | DPUs/host | LAG | Use case |
|------|-------|-----|-----------|-----|----------|
| `single-node.yaml`           | 1 | 1 (both)  | 1 | yes | Laptop / single-server demo (TEST-NET) |
| `single-node-jumphost.yaml`  | 1 | 1 (both)  | 1 | yes | **Operator behind a bastion** — split-identity SSH (workstation key opens jumphost, lab key opens host), `bfb_on_host` pre-staged image, `bnk_forge` registered with linked SSH credential. Derived from the ailab single-node PoC. |
| `two-host-2cp.yaml`          | 2 | 2 (both)  | 1 | yes | Most common lab PoC (TEST-NET) |
| `two-host-cp-worker.yaml`    | 2 | 1         | 1 | yes | Smaller-footprint lab (TEST-NET) |
| `three-host-ha.yaml`         | 3 | 3 (all)   | 1 | yes | HA-safe, production-leaning (TEST-NET) |
| `two-node-homelab.yaml`      | 2 | 1 + worker| 1 | yes | Real working 2-node lab (CUSTOMIZE markers) |

### Picking between `single-node.yaml` and `single-node-jumphost.yaml`

| Question | Pick `single-node.yaml` | Pick `single-node-jumphost.yaml` |
|---|---|---|
| Operator's machine can reach the lab host directly | ✓ | |
| Lab host is behind a bastion / VPN-only / transatlantic | | ✓ |
| Same SSH identity opens bastion + host | (no jumphost at all) | ✓ (leave `jumphost_user` / `jumphost_key_ref` unset → reuses the target key) |
| Different identities per hop (workstation key for bastion, lab-provided key for host) | | ✓ |
| 1.5 GB BFB upload over the operator's link is acceptable | ✓ | |
| BFB should be pre-staged on the lab host (zero WAN bytes for the image) | | ✓ (`provisioning.bfb_on_host`) |
| bnk-forge runs locally and should be auto-registered | works in both, set `bnk_forge.enabled: true` | ✓ (sample sets it on by default) |

Both shapes share the same single-node topology (1 host as control-plane + worker, 1 DPU joined as worker). Pick `-jumphost` whenever the lab host isn't directly routable — the schema fields are no-ops when the operator's machine sits on the same network.

## Placeholder ranges

- **Management network (host SSH):** `192.0.2.0/24` (RFC 5737 TEST-NET-1)
- **External data plane:** `203.0.113.0/24` (RFC 5737 TEST-NET-3)
- **Internal data plane:** `198.51.100.0/24` (RFC 5737 TEST-NET-2)
- **Pod CIDR:** `198.18.100.0/24` (RFC 2544 — dpubnkctl default)
- **DPU tmfifo:** `192.168.100.0/30` per host (host-to-DPU private link)

`single-node-jumphost.yaml` uses realistic private ranges (`10.40.0.x`
external, `10.50.0.x` internal, `10.10.0.x` mgmt, `bastion.example.com`
as the jumphost FQDN) so the operator can read the file and immediately
see the shape. They're still placeholders — replace with the values
from your lab.

Replace **every** address with values from the customer's network plan
before validating.

## Conventions all examples follow

- Host names: `host1`, `host2`, `host3` (positional; replace with real
  hostnames during discovery).
- DPU OS hostnames: `<host-name>-bf3` (e.g. `host1-bf3`).
- DPU PCI address: `0000:03:00.0` (most common slot for a BlueField-3
  in a 1U server; verify with `lspci -nn -d 15b3:` and adjust).
- Per-host data-plane IPs: hosts at `.66/.71/.76` mirroring the mgmt
  last-octet to make the mapping easy to remember.
- Per-DPU data-plane IPs: `.5/.6/.7` (sequential).
- Self-IPs: `.100` in each VLAN (well below the host/DPU range).
