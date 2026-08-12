# Tokyo Lab — dpubnkctl Deployment Procedures

> **Lab:** tky-bnk-dpu-host-2
> **Jumphost:** 172.28.13.128 (ubuntu, ~/shaath/)
> **Host:** 172.28.13.16 (dpu-server-2, ubuntu)
> **DPU:** 192.168.100.2 via tmfifo (dpu-server-2-bf3)
> **External VLAN:** 192.168.40.0/24 (host .16, DPU .5)
> **Internal VLAN:** 192.168.50.0/24 (host .16, DPU .5)

---

## Prerequisites

- Binary built and copied to jumphost
- FAR key + JWT at `~/shaath/f5-far-auth-key.tgz` and `~/shaath/.jwt`
- Post-scripts at `~/shaath/tokyo-lab-post-scripts/`
- For offline: artifacts backup at `~/shaath/artifacts-backup/`

### Build and copy binary (from Mac)

```bash
cd ~/dpubnkctl
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o dpubnkctl-airgap ./cmd/dpubnkctl/
scp ~/dpubnkctl/dpubnkctl-airgap ubuntu@172.28.13.128:~/shaath/dpubnkctl-airgap
```

---

## A. Online Deployment

### On JUMPHOST

```bash
cd ~/shaath

# 1. Clean + Init
sudo rm -rf test-online
./dpubnkctl-airgap init test-online --customer "shaath-test"

# 2. Copy keys
cp ~/shaath/f5-far-auth-key.tgz test-online/keys/
cp ~/shaath/.jwt test-online/keys/.jwt

# 3. Copy post-scripts
cp ~/shaath/scripts/*.sh test-online/post-scripts/
chmod +x test-online/post-scripts/*.sh

# 4. Wizard (wizard.sh auto-fixes poc.yaml)
./dpubnkctl-airgap discover wizard --poc test-online

# 5. Run e2e
screen -S deploy
./dpubnkctl-airgap e2e --yolo --airgap online --poc test-online
# Detach: Ctrl+A then D
# Reattach: screen -r deploy
```

Post-scripts run automatically:
- `wizard.sh` — fixes poc.yaml (DPU count, LAG, parent iface, uplinks, NFS)
- `provision.sh` — configures OVS bridges on DPU via SSH
- `host-network.sh` — applies netplan on host via SSH
- `cluster-up.sh` — sets up kubeconfig on host via SSH

### Verify (on HOST — dpu-server-2)

```bash
kubectl get license.k8s.f5net.com -n f5-cne-core
# STATE=Active

kubectl get f5-spk-vlans.k8s.f5net.com -n f5-bnk
# READY=True

kubectl get gatewayclass
# ACCEPTED=True

kubectl get pods -n f5-bnk -l app=f5-tmm -o wide
# Running, 2/2 readiness gates

kubectl get cneinstance -A
# Available=True
```

### Backup artifacts for future offline runs (on JUMPHOST)

```bash
rm -rf ~/shaath/artifacts-backup
cp -r test-online/artifacts ~/shaath/artifacts-backup
```

### Destroy (on JUMPHOST)

```bash
./dpubnkctl-airgap destroy --yolo --confirm-cluster test-online --poc test-online
```

### Clean up

On JUMPHOST:

```bash
docker system prune -af --volumes
docker builder prune -af
```

On HOST (dpu-server-2):

```bash
sudo rm -rf /etc/containerd/certs.d/
rm -rf $HOME/.kube/
```

---

## B. Offline Deployment

### On JUMPHOST

```bash
cd ~/shaath

# 1. Clean + Init
sudo rm -rf test-offline
./dpubnkctl-airgap init test-offline --customer "shaath-test"

# 2. Copy keys
cp ~/shaath/f5-far-auth-key.tgz test-offline/keys/
cp ~/shaath/.jwt test-offline/keys/.jwt

# 3. Copy post-scripts
cp ~/shaath/scripts/*.sh test-offline/post-scripts/
chmod +x test-offline/post-scripts/*.sh

# 4. Restore artifacts from backup (copy, not move)
cp -r ~/shaath/artifacts-backup/airgap test-offline/artifacts/airgap
cp -r ~/shaath/artifacts-backup/release-manifest test-offline/artifacts/release-manifest

# 5. Verify staging
for d in images images-dpu dpu-debs files charts manifests; do
  printf "%-12s %3d files\n" "$d/" $(ls test-offline/artifacts/airgap/$d/ 2>/dev/null | wc -l)
done
ls test-offline/artifacts/release-manifest/manifest.yaml 2>/dev/null && echo "release-manifest: OK" || echo "release-manifest: MISSING"
# Expected: images 55, images-dpu 29, dpu-debs 6, files 9, charts 4, manifests 1, release-manifest OK

# 6. Wizard (wizard.sh auto-fixes poc.yaml)
./dpubnkctl-airgap discover wizard --poc test-offline

# 7. Run e2e
screen -S deploy
./dpubnkctl-airgap e2e --yolo --airgap offline --poc test-offline
# Detach: Ctrl+A then D
# Reattach: screen -r deploy
```

Post-scripts run automatically (same as online).

### Disconnected License (manual step)

Deploy cne will stop at step 5 and print instructions with the DigitalAssetID and curl command.

**Step 1 — From Mac, copy config report from jumphost:**

```bash
scp ubuntu@172.28.13.128:~/shaath/test-offline/artifacts/license-config-report.json /tmp/config-report.json
```

**Step 2 — From Mac, POST to F5 licensing server (use the curl command printed by deploy cne):**

```bash
curl -sk -X POST https://product.apis.f5.com/ee/v1/entitlements/telemetry \
  -H "Content-Type: application/json" \
  -H "F5-DigitalAssetId: <ASSET_ID_FROM_OUTPUT>" \
  -H "User-Agent: SPK" \
  -H "Authorization: Bearer <JWT_FROM_OUTPUT>" \
  -d @/tmp/config-report.json \
  -o /tmp/license-manifest.json
```

**Step 3 — Verify manifest (expect ~16KB):**

```bash
ls -la /tmp/license-manifest.json
```

**Step 4 — Copy manifest back to jumphost:**

```bash
scp /tmp/license-manifest.json ubuntu@172.28.13.128:~/shaath/license-manifest.json
```

**Step 5 — On JUMPHOST, apply receipt:**

```bash
./dpubnkctl-airgap license apply-receipt --poc test-offline --manifest ~/shaath/license-manifest.json
```

**Step 6 — On HOST (dpu-server-2), if TMM does not reach 2/2 readiness gates:**

```bash
kubectl rollout restart -n f5-bnk daemonset/f5-tmm
kubectl rollout status -n f5-bnk daemonset/f5-tmm --timeout=10m
```

### Verify (on HOST — dpu-server-2)

```bash
kubectl get license.k8s.f5net.com -n f5-cne-core
# STATE=Active

kubectl get f5-spk-vlans.k8s.f5net.com -n f5-bnk
# READY=True

kubectl get gatewayclass
# ACCEPTED=True

kubectl get pods -n f5-bnk -l app=f5-tmm -o wide
# Running, 2/2 readiness gates

kubectl get cneinstance -A
# Available=True
```

### Destroy (on JUMPHOST)

```bash
./dpubnkctl-airgap destroy --yolo --confirm-cluster test-offline --poc test-offline
```

### Clean up

On JUMPHOST:

```bash
docker system prune -af --volumes
docker builder prune -af
```

On HOST (dpu-server-2):

```bash
sudo rm -rf /etc/containerd/certs.d/
rm -rf $HOME/.kube/
```
