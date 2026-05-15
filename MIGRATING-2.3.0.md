# Migrating from BNK 2.2.0 to 2.3.0

This binary is per-BNK-release. `main` carries the current BNK
2.3.0 work plus all post-release maintenance. `release-2.3.0` is the
2.3.x maintenance branch (currently the same tip as `main`). 2.2.x
maintenance lives on `release-2.2.0`. Tags are not used — branch
tips are the canonical reference for "latest for this BNK release".

This document is for operators who have a BNK 2.2.0 PoC repo and want
to redeploy it on 2.3.0 with the new `dpubnkctl`. **There is no
in-place upgrade.** Tear down, swap the binary, edit `poc.yaml`,
re-deploy.

## Pre-flight: confirm a clean exit on 2.2

```bash
cd ~/lab/<your-poc>
dpubnkctl destroy --yolo --confirm-cluster <your-poc>
```

Reboot the worker hosts after destroy. The host-side `mlx5_core`
PF lands in the post-flash "ghost" state when the DPUs reflash on
2.3 (AGENTS.md #9); rebooting the hosts up front avoids a hung
`host network setup` later.

## Swap the binary

Install the new 2.3-targeted binary somewhere on `PATH` (replacing
the 2.2 one is fine — the binary embeds everything):

```bash
go install github.com/mwiget/dpubnkctl/cmd/dpubnkctl@release-2.3.0
dpubnkctl version
# expect:
#   Targets    BNK 2.3.0
#   DOCA / BFB 3.2.0  (bf-bundle-3.2.0-113_25.10_ubuntu-24.04_64k_prod.bfb)
```

## Edit `poc.yaml` for 2.3

Open `poc.yaml` in the PoC repo and update the `versions:` block:

```yaml
versions:
    doca: 3.2.0
    bfb_image: bf-bundle-3.2.0-113_25.10_ubuntu-24.04_64k_prod.bfb
    flo_chart: ""           # resolved at deploy time from release-manifest
    k8s: "1.30"             # 1.32 dropped in BNK 2.3 supported matrix
```

Leave the rest of `poc.yaml` alone. Hosts, DPUs, VLANs, network
plan, FAR/JWT references — all carry over unchanged.

If your `poc.yaml` has a non-default network `vlans[]` shape or
custom DPU `tmfifo_ip`, those stay as-is. The 2.3 schema is
strictly a superset of the 2.2 schema for these fields.

## Things to know before redeploying

1. **Re-flash both DPUs.** DOCA 3.2.0 ships Ubuntu 24.04 with kernel
   6.8; the 2.9.2 BFB cannot be upgraded in place. `dpubnkctl provision
   dpu <host> --yolo --confirm-flash <host>` against each host.
   ~25 min per DPU (~15 min when the 1.5 GB BFB is already cached).

2. **Reboot the host workers after re-flash.** The host-side
   `mlx5_core` PF goes into the post-flash "ghost" state. `host
   network setup` detects this and refuses; the fix is a host reboot.
   (Audited fix #11 from the 2.2 PoC; carries over to 2.3.)

3. **Licensing moved.** The JWT no longer lives in `flo-values.yaml`.
   `deploy flo` installs an empty FLO chart (no license, no TEEM
   URLs, no x5c chain). After `deploy cne` brings up CWC, the binary
   applies a `License` CR (`k8s.f5net.com/v1`) into the
   `f5-cne-core` namespace. CWC validates the JWT against the TEEM
   endpoint derived from the JWT's `jku` header — so **prod vs tst
   is now automatic**; no per-environment template selection.

   For a tst PoC: `kubectl -n f5-cne-core get license` will show
   `ENVIRONMENT=test` once CWC finishes registration (~5-15 min on
   first deploy).

4. **`gateway example` now emits `hostnames:`.** BNK 2.3 Gateway API
   Conformance enhancements made HTTPRoutes without a hostname stop
   matching catch-all traffic. The scaffolded example now ships with
   `hostnames: ["<app>.local"]`; curl with `-H "Host: <app>.local"`
   or set up DNS pointing at the Gateway's `spec.addresses` value.

5. **`release-manifest pull` is new.** `dpubnkctl release-manifest
   pull` prints the resolved chart and image versions for the BNK
   release without writing anything to the cluster. Useful pre-deploy
   sanity check; required for nothing else.

## Redeploy

End-to-end, resumable:

```bash
dpubnkctl e2e --yolo --no-resume
```

Or per phase, in order:

```bash
dpubnkctl provision dpu worker1 worker2 --yolo --confirm-flash worker1,worker2
# reboot host workers
dpubnkctl host network setup --yolo --confirm-cluster <name>
dpubnkctl cluster up --yolo --confirm-cluster <name>
dpubnkctl cluster join-dpus --yolo --confirm-cluster <name>
dpubnkctl deploy network --yolo --confirm-deploy <name>
dpubnkctl deploy flo --yolo --confirm-deploy <name>
dpubnkctl deploy cne --yolo --confirm-deploy <name>
```

Expect ~60 minutes wall-clock from a clean state.

## Verify

```bash
# Cluster + BNK control plane up:
dpubnkctl cluster status

# License Active:
kubectl -n f5-cne-core get license

# TMM running on every DPU node:
kubectl -n default get pods -l app=f5-tmm -o wide

# Smoke-test end-to-end:
dpubnkctl gateway example --smoke-test | kubectl apply -f -
ssh ubuntu@<worker-on-external-vlan> "curl -H 'Host: demo-app.local' http://<bnk.external_selfip>/"
# expect: HTTP/1.1 200 OK
```

## Rolling back to 2.2

If 2.3 doesn't work out:

1. `dpubnkctl destroy --yolo --confirm-cluster <name>` (with the 2.3
   binary — needed to clean up the new `f5-cne-core` namespace + the
   `License` CR finalizer).
2. Reboot host workers.
3. Re-flash both DPUs with the BNK 2.2 BFB
   (`bf-bundle-2.9.2-32_25.02_ubuntu-22.04_prod.bfb`) using the
   `release-2.2.0` branch's binary.
4. Restore the 2.2 `versions:` block in `poc.yaml`.
5. Re-run `dpubnkctl e2e --yolo --no-resume`.

The NIC firmware on the DPU was ratcheted to `32.47.1026` by the 2.3
BFB; rollback to the 2.9.2 NIC firmware would require an explicit
`mlxfwmanager --downgrade` on each DPU. Not normally needed — the
newer NIC firmware is backward-compatible with the 2.9.2 host stack.
