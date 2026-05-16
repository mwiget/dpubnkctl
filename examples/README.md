# Example PoC shapes

Pre-canned `poc.yaml` skeletons matching the topologies in
`dpubnkctl topologies`. Embedded in the binary — list with
`dpubnkctl samples`, extract with `dpubnkctl samples extract <name>`
or seed a fresh PoC with `dpubnkctl init <poc-name> --sample <name>`.

The TEST-NET shapes use RFC 5737 placeholder addresses (safe to
commit, obvious to spot when un-edited in a real lab). The
`two-node-homelab` shape uses realistic private ranges
(`192.168.10.x`, `10.40.0.x`, `10.50.0.x`) because it's derived
from a working 2-node homelab — every `# CUSTOMIZE:` comment marks
exactly the fields the operator must change.

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
| `single-node.yaml`       | 1 | 1 (both)  | 1 | yes | Laptop / single-server demo (TEST-NET) |
| `two-host-2cp.yaml`      | 2 | 2 (both)  | 1 | yes | Most common lab PoC (TEST-NET) |
| `two-host-cp-worker.yaml`| 2 | 1         | 1 | yes | Smaller-footprint lab (TEST-NET) |
| `three-host-ha.yaml`     | 3 | 3 (all)   | 1 | yes | HA-safe, production-leaning (TEST-NET) |
| `two-node-homelab.yaml`  | 2 | 1 + worker| 1 | yes | Real working 2-node lab (CUSTOMIZE markers) |

## Placeholder ranges

- **Management network (host SSH):** `192.0.2.0/24` (RFC 5737 TEST-NET-1)
- **External data plane:** `203.0.113.0/24` (RFC 5737 TEST-NET-3)
- **Internal data plane:** `198.51.100.0/24` (RFC 5737 TEST-NET-2)
- **Pod CIDR:** `198.18.100.0/24` (RFC 2544 — dpubnkctl default)
- **DPU tmfifo:** `192.168.100.0/30` per host (host-to-DPU private link)

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
