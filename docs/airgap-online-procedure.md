# Airgap Online Deployment Procedure

> dpubnkctl deploys BNK 2.3 on bare-metal with NVIDIA BlueField-3 DPUs.
> **Online mode:** Jumphost has internet. Downloads images, binaries, and charts during Phase 0, stages them locally, then deploys without internet on cluster nodes.

---

## Prerequisites

- Jumphost with internet access, Docker, and skopeo installed
- SSH access to the host (control-plane server)
- DPU connected via tmfifo (192.168.100.x)
- FAR key (`f5-far-auth-key.tgz`) and JWT (`.jwt`) from F5
- Post-scripts at `~/shaath/scripts/` (optional — automate lab-specific steps)

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

### 5. Discover wizard

```bash
./dpubnkctl-airgap discover wizard --poc <poc-name>
```

If `wizard.sh` post-script is present, poc.yaml fixes are applied automatically.

### 6. Run e2e

```bash
screen -S deploy
./dpubnkctl-airgap e2e --yolo --airgap online --poc <poc-name>
# Detach: Ctrl+A then D
# Reattach: screen -r deploy
```

Post-scripts run automatically after each phase. If no post-scripts are present, the manual steps below apply.

### 7. Manual steps (only if NOT using post-scripts)

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

### 8. Verify deployment

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

### 9. Backup artifacts for future offline runs

```bash
rm -rf ~/shaath/artifacts-backup
cp -r <poc-name>/artifacts ~/shaath/artifacts-backup
```

### 10. Destroy

```bash
./dpubnkctl-airgap destroy --yolo --confirm-cluster <poc-name> --poc <poc-name>
```

### 11. Clean up

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
