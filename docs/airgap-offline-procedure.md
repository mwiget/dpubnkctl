# Airgap Offline Deployment Procedure

> dpubnkctl deploys BNK 2.3 on bare-metal with NVIDIA BlueField-3 DPUs.
> **Offline mode:** Jumphost has NO internet. Uses pre-staged packages from a prior online run. No downloads during deployment.

---

## Prerequisites

- Jumphost running Ubuntu 22.04+ (no internet required)
- SSH access to the host (control-plane server) via key-based auth
- DPU connected via tmfifo (192.168.100.x)
- FAR key (`f5-far-auth-key.tgz`) and JWT (`.jwt`) from F5
- Post-scripts at `~/shaath/scripts/` (optional — automate lab-specific steps)
- Artifacts backup from a prior online run (see "Downloading Images" below)

---

## Jumphost Preparation

Docker must be installed on the jumphost **before** going offline. Install it while the jumphost still has internet, or from a local `.deb` package.

### Install Docker (while internet is available)

```bash
sudo apt-get update
sudo apt-get install -y ca-certificates curl gnupg

sudo install -m 0755 -d /etc/apt/keyrings
curl -fsSL https://download.docker.com/linux/ubuntu/gpg | sudo gpg --dearmor -o /etc/apt/keyrings/docker.gpg
sudo chmod a+r /etc/apt/keyrings/docker.gpg

echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] \
  https://download.docker.com/linux/ubuntu $(. /etc/os-release && echo "$VERSION_CODENAME") stable" | \
  sudo tee /etc/apt/sources.list.d/docker.list > /dev/null

sudo apt-get update
sudo apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin

# Allow current user to run docker without sudo
sudo usermod -aG docker $USER
newgrp docker

# Verify
docker run --rm hello-world
```

### Install sshpass (needed by post-scripts for DPU access)

```bash
sudo apt-get install -y sshpass
```

> **Note:** skopeo is NOT required for offline mode — it is only used during the online image download phase.

### Verify

```bash
docker version
ssh -V
```

---

## Downloading Images (One-Time Setup)

Before running offline mode, you need a complete artifacts backup. This is done once from a machine with internet access, then the backup is carried to the air-gapped jumphost.

### On a machine with internet (jumphost or staging machine)

```bash
# 1. Init a temporary PoC
./dpubnkctl-airgap init staging --customer "staging"

# 2. Copy keys (needed for FAR-authenticated image pulls)
cp f5-far-auth-key.tgz staging/keys/
cp .jwt staging/keys/.jwt

# 3. Run the wizard (minimal — just needs a valid poc.yaml)
./dpubnkctl-airgap discover wizard --poc staging

# 4. Run Phase 0 online — downloads all images, binaries, charts
./dpubnkctl-airgap airgap setup --airgap online --poc staging

# 5. Backup the artifacts
cp -r staging/artifacts ~/artifacts-backup

# 6. Verify
for d in images images-dpu dpu-debs files charts manifests; do
  printf "%-12s %3d files\n" "$d/" $(ls ~/artifacts-backup/airgap/$d/ 2>/dev/null | wc -l)
done
ls ~/artifacts-backup/release-manifest/manifest.yaml 2>/dev/null && echo "release-manifest: OK" || echo "release-manifest: MISSING"
# Expected: images 55, images-dpu 29, dpu-debs 6, files 9, charts 4, manifests 1, release-manifest OK

# 7. Clean up staging
rm -rf staging
```

### Transfer to air-gapped jumphost

Copy the `artifacts-backup/` directory to the air-gapped jumphost via USB, SCP, or any available transfer method. Place it at `~/shaath/artifacts-backup/` (or any path — referenced in the restore step below).

---

## Post-Scripts (Optional)

Post-scripts automate lab-specific manual steps. Place them in `<poc-name>/post-scripts/` and they run automatically after each e2e phase. Script names match phase names:

| Script | Runs after | What it does |
|--------|-----------|--------------|
| `wizard.sh` | discover wizard | Fix poc.yaml (DPU count, LAG, interfaces, NFS) |
| `provision.sh` | provision | SSH to DPU, configure OVS bridges |
| `host-network.sh` | host-network | SSH to host, apply netplan |
| `cluster-up.sh` | cluster-up | SSH to host, set up kubeconfig |

Scripts run on the jumphost. If they need to act on remote hosts, they SSH within the script. Non-zero exit code fails the phase.

---

## Procedure

### 1. Build and copy binary (from Mac)

```bash
cd ~/dpubnkctl
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o dpubnkctl-airgap ./cmd/dpubnkctl/
scp ~/dpubnkctl/dpubnkctl-airgap ubuntu@172.28.13.128:~/shaath/dpubnkctl-airgap
```

### 2. Init PoC

```bash
cd ~/shaath
sudo rm -rf <poc-name>
./dpubnkctl-airgap init <poc-name> --customer "<customer-name>"
```

### 3. Copy keys

```bash
cp ~/shaath/f5-far-auth-key.tgz <poc-name>/keys/
cp ~/shaath/.jwt <poc-name>/keys/.jwt
```

### 4. Copy post-scripts (optional)

```bash
cp ~/shaath/scripts/*.sh <poc-name>/post-scripts/
chmod +x <poc-name>/post-scripts/*.sh
```

### 5. Restore artifacts from backup

Copy (not move) the backed-up artifacts. Only `airgap/` and `release-manifest/` are needed.

```bash
cp -r ~/shaath/artifacts-backup/airgap <poc-name>/artifacts/airgap
cp -r ~/shaath/artifacts-backup/release-manifest <poc-name>/artifacts/release-manifest
```

### 6. Verify staging

```bash
for d in images images-dpu dpu-debs files charts manifests; do
  printf "%-12s %3d files\n" "$d/" $(ls <poc-name>/artifacts/airgap/$d/ 2>/dev/null | wc -l)
done
ls <poc-name>/artifacts/release-manifest/manifest.yaml 2>/dev/null && echo "release-manifest: OK" || echo "release-manifest: MISSING"
```

Expected: images 55, images-dpu 29, dpu-debs 6, files 9, charts 4, manifests 1, release-manifest OK.

### 7. Discover wizard

```bash
./dpubnkctl-airgap discover wizard --poc <poc-name>
```

If `wizard.sh` post-script is present, poc.yaml fixes are applied automatically.

### 8. Run offline e2e

```bash
screen -S deploy
./dpubnkctl-airgap e2e --yolo --airgap offline --poc <poc-name>
# Detach: Ctrl+A then D
# Reattach: screen -r deploy
```

Post-scripts run automatically after each phase. If no post-scripts are present, the manual steps below apply.

### 9. Manual steps (only if NOT using post-scripts)

**After Phase 3 (provisioning) — configure OVS bridges on DPU:**

On the JUMPHOST — get DPU password:

```bash
cat ~/shaath/<poc-name>/keys/dpu_password.txt
```

On the HOST — SSH to DPU:

```bash
ssh-keygen -f "/home/ubuntu/.ssh/known_hosts" -R "192.168.100.2"
export SSHPASS='PASSWORD_YOU_GOT'
sshpass -e ssh -o StrictHostKeyChecking=no ubuntu@192.168.100.2
```

On the DPU:

```bash
sudo ovs-vsctl del-br sf-external
sudo ovs-vsctl del-br sf-internal

sudo ovs-vsctl add-br sf-external
sudo ovs-vsctl add-port sf-external p0
sudo ovs-vsctl add-port sf-external en3f0pf0sf1
sudo ovs-vsctl add-port sf-external pf0hpf

sudo ovs-vsctl add-br sf-internal
sudo ovs-vsctl add-port sf-internal p1
sudo ovs-vsctl add-port sf-internal en3f1pf1sf1
sudo ovs-vsctl add-port sf-internal pf1hpf

sudo ip link set sf-external up
sudo ip link set sf-internal up
sudo ip addr add 192.168.40.5/24 dev sf-external
sudo ip addr add 192.168.50.5/24 dev sf-internal

sudo ovs-vsctl show
ip a | grep sf-
```

**After Phase 4 (host networking) — apply netplan on HOST:**

```bash
sudo tee /etc/netplan/70-dpubnkctl-dataplane.yaml << 'EOF'
network:
  version: 2
  renderer: networkd
  ethernets:
    enp13s0f0np0:
      mtu: 9000
      addresses:
        - 192.168.40.16/24
    enp13s0f1np1:
      mtu: 9000
      addresses:
        - 192.168.50.16/24
EOF
sudo chmod 600 /etc/netplan/70-dpubnkctl-dataplane.yaml
sudo netplan apply
sudo ip link del external40 2>/dev/null
sudo ip link del internal50 2>/dev/null

ping 192.168.40.5 -c 2
ping 192.168.50.5 -c 2
```

**After Phase 5 (cluster up) — set up kubeconfig on HOST:**

```bash
mkdir -p $HOME/.kube && sudo cp /etc/kubernetes/admin.conf $HOME/.kube/config && sudo chown ubuntu:ubuntu $HOME/.kube/config
```

### 10. Disconnected license (manual step)

In offline mode, the jumphost has no internet, so the license flow pauses after extracting the config report. `deploy cne` will print the DigitalAssetID and a curl command.

**Step 1: Copy config report from jumphost to an internet-connected machine (e.g. your Mac)**

```bash
scp ubuntu@172.28.13.128:~/shaath/<poc-name>/artifacts/license-config-report.json /tmp/config-report.json
```

**Step 2: Run the curl command printed by deploy cne (on the internet-connected machine)**

```bash
curl -sk -X POST https://product.apis.f5.com/ee/v1/entitlements/telemetry \
  -H "Content-Type: application/json" \
  -H "F5-DigitalAssetId: <ASSET_ID_FROM_OUTPUT>" \
  -H "User-Agent: SPK" \
  -H "Authorization: Bearer <JWT_FROM_OUTPUT>" \
  -d @/tmp/config-report.json \
  -o /tmp/license-manifest.json
```

**Step 3: Verify the manifest (expect ~16KB)**

```bash
ls -la /tmp/license-manifest.json
```

**Step 4: Copy manifest back to the jumphost**

```bash
scp /tmp/license-manifest.json ubuntu@172.28.13.128:~/shaath/license-manifest.json
```

**Step 5: On the jumphost, apply the receipt**

```bash
./dpubnkctl-airgap license apply-receipt --poc <poc-name> --manifest ~/shaath/license-manifest.json
```

**Step 6: If TMM does not reach 2/2 readiness gates (on HOST)**

```bash
kubectl rollout restart -n f5-bnk daemonset/f5-tmm
kubectl rollout status -n f5-bnk daemonset/f5-tmm --timeout=10m
```

### 11. Verify deployment

On the HOST:

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

### 12. Destroy

```bash
./dpubnkctl-airgap destroy --yolo --confirm-cluster <poc-name> --poc <poc-name>
```

### 13. Clean up

On JUMPHOST:

```bash
docker system prune -af --volumes
docker builder prune -af
```

On HOST:

```bash
sudo rm -rf /etc/containerd/certs.d/
rm -rf $HOME/.kube/
```
