# dpubnkctl Airgap — Next Session Prompt

Copy this as context for the next Claude Code session.

---

## Project Overview

dpubnkctl is a Go CLI tool (`~/dpubnkctl`) that deploys F5 BNK 2.3 on bare-metal servers with NVIDIA BlueField-3 DPUs. We're adding airgap/offline deployment support.

## Current State

### Airgap Online Mode — WORKING

First successful e2e deployment completed on Tokyo lab (2026-08-10). TMM 7/7 Running, demo app working end-to-end.

**Key architecture:**
- Jumphost (172.28.13.128): runs `dpubnkctl-airgap`, has internet, runs Docker registry + file server
- Host/dpu-server-2 (172.28.13.16): K8s control plane, runs kubectl. NO internet.
- DPU/dpu-server-2-bf3: BlueField-3 DPU worker node. Reached via SSH from host (192.168.100.2 tmfifo)

**Critical fixes that made it work:**
1. Default route on DPU (`default via 192.168.100.1 dev tmfifo_net0 metric 1025`) in `internal/cluster/join.go` — without this, calico proxy ARP doesn't work and pods can't send any traffic
2. Calico deployed via tigera-operator (not kubespray's built-in calico) with `VXLANCrossSubnet` and `nodeAddressAutodetectionV4: kubernetes: NodeInternalIP`
3. `kube_proxy_mode: iptables` (matches offline guide, not ipvs)
4. `kube_network_plugin: none` in kubespray inventory (calico deployed separately)
5. Server-side apply (`kubectl apply --server-side --force-conflicts`) for tigera-operator manifest (262KB CRD annotation limit)
6. Multus restart after calico ready (CNI config rescan)
7. DNS pod restart after calico ready (coredns + dns-autoscaler stuck from before calico)
8. NFS CSI driver via helm + NFS StorageClass (replaces local-path-provisioner)
9. f5-bnk namespace for all BNK resources
10. far-secret created in f5-bnk namespace (FLO needs it there)

### Airgap Offline Mode — NOT TESTED

Same binary, `--airgap offline` flag. Instead of downloading, it loads from pre-staged packages. Code exists but untested on the lab.

## Remaining Work

### 1. Disconnected License Automation (PRIORITY)

**Problem:** In airgap mode, the cluster can't reach F5 licensing servers. The license stays at PendingVerification. Currently dpubnkctl just warns and proceeds — TMM runs but unlicensed.

**Solution:** Automate the 8-step disconnected license flow from the offline guide (`~/bnk-implementation-guide/BNK-2.3-Lab-Guide-tokoyo_v0.4-OFFLINE.md`, section 6.10, lines 1040-1127).

**The flow:**
1. Apply License CR with `operationMode: disconnected` (auto-set when airgap active)
2. Wait for PendingVerification
3. SSH to host, extract CWC client certs from K8s secrets
4. SSH to host, port-forward to CWC (38081), get auth token
5. SSH to host, download config report from CWC `/report`
6. SCP config-report.json from host to jumphost
7. POST config report to F5 licensing server FROM JUMPHOST (has internet): `https://product.apis.f5.com/ee/v1/entitlements/telemetry`
8. SCP license-manifest.json from jumphost to host
9. SSH to host, POST manifest to CWC `/receipt` (ONE SHOT — do not retry)
10. Verify license reaches Active

**Verification at each step:**
- CWC certs: all 3 files exist and non-empty
- Auth token: non-empty string
- Port-forward: curl responds within 10s (retry 3x)
- /status: JSON contains DigitalAssetID
- config-report.json: exists AND size > 0
- license-manifest.json: exists AND size > 100 bytes
- /receipt: HTTP response not error
- License state: Active within 5 minutes

**Files to modify:**
- `internal/cli/deploy_cne.go` — `applyLicenseCR()`: auto-set disconnected when airgap, call `runDisconnectedLicense()` after PendingVerification
- Existing SSH utilities: `sshConfigForHost()`, `ssh.Dial()`, `ssh.Run()` in `internal/cli/` and `internal/ssh/`

**Plan file:** `~/dpubnkctl/docs/disconnected-license-plan.md` (detailed plan with verification steps)

### 2. F5SPKVlan self-IPs — NO CHANGE for now

Tokyo lab has specific topology. VLANs applied manually. Not a code change.

### 3. File Server Timing

Validate sometimes fails on first run (file server not ready). Works on retry. Low priority.

### 4. Node-labeler

Still crashes (pod-to-service routing from DPU pods broken — different from the default route fix which fixed proxy ARP). TMM works without node-labeler completing. The labels (`pf0-mac`, `pf1-mac`, `dpu-sn`, `app=f5-tmm`) are set by node-labeler's first run before it crashes trying to reach the API. Cosmetic issue — doesn't block functionality.

## Reference Commands

### Build

```bash
# Mac — build local (Mac arm64)
cd ~/dpubnkctl
go build ./cmd/dpubnkctl/

# Mac — build Linux binary for jumphost
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o dpubnkctl-airgap ./cmd/dpubnkctl/

# Mac — run tests
go test ./...

# Mac — verify binary contents
grep -ao "pattern" ~/dpubnkctl/dpubnkctl-airgap
```

### Copy binary

```bash
# Mac → Jumphost
scp ~/dpubnkctl/dpubnkctl-airgap ubuntu@172.28.13.128:shaath/dpubnkctl-airgap

# Mac → Host (Tokyo lab, direct SSH via VPN)
scp ~/dpubnkctl/dpubnkctl-airgap ubuntu@172.28.13.16:~/
```

### Jumphost commands (run by Mo on jumphost ~/shaath)

```bash
# Verify binary
grep -ao "pattern" ~/shaath/dpubnkctl-airgap

# Init
./dpubnkctl-airgap init <poc-name> --customer "shaath-test"

# Wizard
./dpubnkctl-airgap discover wizard --poc <poc-name>

# Copy keys
cp ~/shaath/f5-far-auth-key.tgz <poc-name>/keys/
cp ~/shaath/.jwt <poc-name>/keys/.jwt

# Full e2e (run inside screen)
screen -S deploy
./dpubnkctl-airgap e2e --yolo --airgap online --poc <poc-name>
# Detach: Ctrl+A then D
# Reattach: screen -r deploy

# Destroy
./dpubnkctl-airgap destroy --yolo --confirm-cluster <poc-name> --poc <poc-name>

# Individual phases
./dpubnkctl-airgap deploy network --yolo --confirm-deploy <poc-name> --airgap online --poc <poc-name>
./dpubnkctl-airgap deploy cne --yolo --confirm-deploy <poc-name> --airgap online --poc <poc-name>
```

### Host commands (run by Mo on dpu-server-2)

```bash
# SSH to host from Mac
ssh ubuntu@172.28.13.16

# kubectl (only available here)
kubectl get pods -A -o wide
kubectl get nodes -o wide
kubectl get cneinstance -A

# SSH to DPU from host using sshpass
export SSHPASS='<dpu-password>'  # changes per BFB flash, check poc dir
sshpass -e ssh -o StrictHostKeyChecking=no ubuntu@192.168.100.2 "<command>"

# Example: check routes on DPU
sshpass -e ssh -o StrictHostKeyChecking=no ubuntu@192.168.100.2 "ip route"
```

### Code location

```
~/dpubnkctl/
├── cmd/dpubnkctl/          # CLI entry point
├── internal/
│   ├── cli/                # All phase commands (deploy_cne.go, deploy_network.go, etc.)
│   ├── cluster/            # kubespray inventory (inventory.go), join logic (join.go)
│   ├── deploy/             # Template rendering (cne.go, flo.go, license_cr.go, runner.go)
│   ├── embedded/templates/ # YAML templates (cne-instance, f5spkvlan, calico, multus, etc.)
│   ├── airgap/             # Airgap download/config (download.go, config.go)
│   ├── poc/                # poc.yaml schema + validation
│   ├── ssh/                # SSH client utilities
│   └── version/            # Version pins, image lists
├── offline-manifests/      # Reference manifests from manual offline guide
├── examples/               # Sample poc.yaml files
└── docs/                   # Plans and documentation
```

## Important Rules

1. **DO NOT make code changes without Mo's explicit approval** — discuss first, explain why, get confirmation
2. **DO NOT SSH to any remote host** — all commands are for Mo to run, state which machine
3. **Three machines:** Mac (code), Jumphost ~/shaath (dpubnkctl-airgap), dpu-server-2 (kubectl). Never mix.
4. **DO NOT guess paths or commands** — only reference what's been shown
5. **Match the offline guide** at `~/bnk-implementation-guide/BNK-2.3-Lab-Guide-tokoyo_v0.4-OFFLINE.md` — it's the proven reference
