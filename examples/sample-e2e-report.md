# e2e report — lake1-test

- **Started:** 2026-05-13T19:59:47Z
- **Finished:** 2026-05-13T19:59:53Z
- **Wall:** 6s

**Result:** 2 ok, 0 failed, 6 skipped

## Versions

- **dpubnkctl:** 300fe8e  (targets BNK 2.2.0)
- **FLO chart:** v2.9.27-0.2.10
- **CNE manifest:** 2.2.0-3.2226.0-0.0.385

## Topology

```
K8s cluster: lake1-test
=======================

  +----------------------+     +----------------------+ 
  |       worker1        |     |       worker2        | 
  |    control-plane     |     |        worker        | 
  |    192.168.68.66     |     |    192.168.68.71     | 
  +----------------------+     +----------------------+ 
             |                            |             
             | kubeadm join               | kubeadm join
             v                            v             
  +----------------------+     +----------------------+ 
  |     worker1-bf3      |     |     worker2-bf3      | 
  |      DPU worker      |     |      DPU worker      | 
  |        (LAG)         |     |        (LAG)         | 
  |  mgmt 192.168.68.96  |     |  mgmt 192.168.68.79  | 
  +----------------------+     +----------------------+ 

  apiserver: 10.10.41.66:6443  (all 4 node(s) connect here)

Data-plane VLANs
================

  external VLAN 40 — 10.10.40.0/24
  ----------------------------------
    worker1        .66
    worker1-bf3    .5
    worker2        .71
    worker2-bf3    .6
    TMM self-IP    .100

  internal VLAN 41 — 10.10.41.0/24
  ----------------------------------
    worker1        .66
    worker1-bf3    .5
    worker2        .71
    worker2-bf3    .6
    TMM self-IP    .100

```

## Phases

| # | Phase | Status | Duration | Log |
|---|---|---|---|---|
| 1 | validate | ✅ ok | 0s | `logs/01-validate.log` |
| 2 | provision | ⏭️ skipped | — | — |
| 3 | host-network | ⏭️ skipped | — | — |
| 4 | cluster-up | ⏭️ skipped | — | — |
| 5 | cluster-join-dpus | ⏭️ skipped | — | — |
| 6 | deploy-network | ⏭️ skipped | — | — |
| 7 | deploy-flo | ⏭️ skipped | — | — |
| 8 | deploy-cne | ✅ ok | 6s | `logs/08-deploy-cne.log` |

## Per-phase summary

### 1. validate — ok

    /home/mwiget/git/dpubnkctl/bin/dpubnkctl validate --poc /tmp/dpubnkctl-lake1-test

completed in 0s

### 2. provision — skipped

resumed: previously completed at 2026-05-13T18:33:08Z (--no-resume to re-run)

### 3. host-network — skipped

resumed: previously completed at 2026-05-13T18:33:14Z (--no-resume to re-run)

### 4. cluster-up — skipped

resumed: previously completed at 2026-05-13T19:26:57Z (--no-resume to re-run)

### 5. cluster-join-dpus — skipped

resumed: previously completed at 2026-05-13T19:28:25Z (--no-resume to re-run)

### 6. deploy-network — skipped

resumed: previously completed at 2026-05-13T19:28:43Z (--no-resume to re-run)

### 7. deploy-flo — skipped

resumed: previously completed at 2026-05-13T19:39:56Z (--no-resume to re-run)

### 8. deploy-cne — ok

    /home/mwiget/git/dpubnkctl/bin/dpubnkctl deploy cne --poc /tmp/dpubnkctl-lake1-test --yolo --confirm-deploy lake1-test

completed in 6s

