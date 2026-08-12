# dpubnkctl Air-Gap Deployment Guide

> Tested and validated on UDF (2026-08-07). This guide covers deploying BNK 2.3 on the Tokyo lab server (tky-bnk-dpu-host-2) using a VM jumphost with internet access.

---

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│  VM Jumphost (internet-connected)                           │
│                                                             │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────────┐  │
│  │ Container    │  │ File Server  │  │ dpubnkctl        │  │
│  │ Registry     │  │ (nginx:80)   │  │ (Go binary)      │  │
│  │ (TLS:5000)   │  │              │  │                  │  │
│  │              │  │ K8s binaries │  │ kubespray inside │  │
│  │ K8s images   │  │ containerd   │  │ Docker container │  │
│  │ Calico imgs  │  │ runc, crictl │  │                  │  │
│  │ F5 BNK imgs  │  │ CNI plugins  │  │                  │  │
│  └──────┬───────┘  └──────┬───────┘  └────────┬─────────┘  │
│         │                 │                    │            │
│         └─────────────────┼────────────────────┘            │
│                           │                                 │
│                     Management Network                      │
└───────────────────────────┬─────────────────────────────────┘
                            │
              ┌─────────────┼─────────────────┐
              │             │                 │
     ┌────────▼───────┐  ┌──▼──────────┐  ┌──▼──────────┐
     │  BNK Host      │  │  DPU-1      │  │  DPU-N      │
     │  (control-     │  │  (worker    │  │  (worker    │
     │   plane)       │  │   node)     │  │   node)     │
     │                │  │             │  │             │
     │  Pulls from    │  │  Images via │  │  Images via │
     │  registry +    │  │  SCP + ctr  │  │  SCP + ctr  │
     │  file server   │  │  import     │  │  import     │
     └────────────────┘  └─────────────┘  └─────────────┘
```

**Key principle:** Target servers (host + DPUs) never touch the internet. The jumphost is the single source for everything.

---

## Prerequisites

### Jumphost (VM with internet)

- Ubuntu 22.04
- Docker installed
- skopeo installed
- SSH access to BNK host server
- FAR credentials (`cne_pull_64.json`) for F5 image access

### Target Server (Tokyo: tky-bnk-dpu-host-2)

- BNK host with BlueField-3 DPUs
- SSH accessible from jumphost over management network
- No internet required

---

## Phase 0 — Prepare Offline Infrastructure

### 0.1 Install tools on jumphost

```bash
# Docker
curl -fsSL https://get.docker.com | sh
sudo usermod -aG docker $USER
# Log out and back in

# skopeo
sudo apt-get update
sudo apt-get install -y skopeo git
```

### 0.2 Clone repos

```bash
cd ~/shaath
git clone --branch release-2.3.0 https://github.com/mwiget/dpubnkctl.git
git clone --branch v2.28.1 https://github.com/kubernetes-sigs/kubespray.git
docker pull quay.io/kubespray/kubespray:v2.28.1
```

### 0.3 Generate TLS certificates

```bash
mkdir -p ~/shaath/airgap/certs
JUMPHOST_IP="<jumphost-management-ip>"

openssl req -x509 -newkey rsa:4096 \
  -keyout ~/shaath/airgap/certs/registry.key \
  -out ~/shaath/airgap/certs/registry.crt \
  -days 365 -nodes \
  -subj "/CN=${JUMPHOST_IP}" \
  -addext "subjectAltName=IP:${JUMPHOST_IP}"
```

### 0.4 Start local container registry

```bash
docker run -d --name registry --restart=always \
  -p 5000:5000 \
  -v ~/shaath/airgap/certs:/certs \
  -e REGISTRY_HTTP_TLS_CERTIFICATE=/certs/registry.crt \
  -e REGISTRY_HTTP_TLS_KEY=/certs/registry.key \
  registry:2
```

### 0.5 Download and load kubespray images

Resolve exact versions kubespray needs for K8s 1.30:

```bash
# Create minimal inventory to resolve versions
mkdir -p ~/shaath/kubespray/inventory/bnk-airgap/group_vars/all
mkdir -p ~/shaath/kubespray/inventory/bnk-airgap/group_vars/k8s_cluster

cat > ~/shaath/kubespray/inventory/bnk-airgap/hosts.yaml << 'EOF'
all:
  hosts:
    tky-bnk-dpu-host-2:
      ansible_host: <host-management-ip>
      ip: <host-management-ip>
  children:
    kube_control_plane:
      hosts:
        tky-bnk-dpu-host-2:
    kube_node:
      hosts: {}
    etcd:
      hosts:
        tky-bnk-dpu-host-2:
    k8s_cluster:
      children:
        kube_control_plane:
        kube_node:
    calico_rr:
      hosts: {}
EOF

cat > ~/shaath/kubespray/inventory/bnk-airgap/group_vars/k8s_cluster/k8s-cluster.yml << 'EOF'
kube_version: 1.30.14
kube_network_plugin: calico
container_manager: containerd
EOF

cat > ~/shaath/kubespray/inventory/bnk-airgap/group_vars/all/all.yml << 'EOF'
containerd_version: 1.7.23
etcd_deployment_type: host
EOF
```

Download and push kubespray container images to local registry:

```bash
mkdir -p ~/shaath/airgap/images

# Kubespray images (versions resolved from kubespray v2.28.1 with K8s 1.30)
KUBESPRAY_IMAGES=(
  "registry.k8s.io/kube-apiserver:v1.30.14"
  "registry.k8s.io/kube-controller-manager:v1.30.14"
  "registry.k8s.io/kube-scheduler:v1.30.14"
  "registry.k8s.io/kube-proxy:v1.30.14"
  "registry.k8s.io/coredns/coredns:v1.11.3"
  "registry.k8s.io/pause:3.9"
  "registry.k8s.io/pause:3.10"
  "registry.k8s.io/dns/k8s-dns-node-cache:1.25.0"
  "registry.k8s.io/cpa/cluster-proportional-autoscaler:v1.8.8"
  "quay.io/coreos/etcd:v3.5.22"
  "quay.io/calico/node:v3.29.5"
  "quay.io/calico/cni:v3.29.5"
  "quay.io/calico/kube-controllers:v3.29.5"
  "quay.io/calico/typha:v3.29.5"
  "quay.io/calico/apiserver:v3.29.5"
  "docker.io/library/nginx:1.27.4-alpine"
  "docker.io/library/haproxy:3.1.3-alpine"
  "docker.io/library/registry:2.8.1"
  "docker.io/rancher/local-path-provisioner:v0.0.24"
)

for img in "${KUBESPRAY_IMAGES[@]}"; do
  name=$(echo "$img" | sed 's|.*/||; s/:/-/g')
  short=$(echo "$img" | sed 's|^registry\.k8s\.io/||; s|^quay\.io/||; s|^docker\.io/||; s|^ghcr\.io/||')
  echo "Pulling $img ..."
  skopeo copy --override-arch amd64 docker://$img docker-archive:$HOME/airgap/images/${name}.tar:${img}
  echo "Pushing -> 10.1.1.4:5000/${short}"
  skopeo copy --dest-tls-verify=false docker-archive:$HOME/airgap/images/${name}.tar docker://${JUMPHOST_IP}:5000/${short}
done
```

### 0.6 Download and load BNK images (F5, amd64 + arm64)

```bash
# Login to FAR
skopeo login repo.f5.com \
  --username _json_key_base64 \
  --password "$(cat ~/shaath/cne_pull_64.json)"

# Host images (amd64) — F5 BNK components
BNK_HOST_IMAGES=(
  "repo.f5.com/images/crd-conversion:v1.250.3"
  "repo.f5.com/images/crd-installer:v14.59.1-0.0.70"
  "repo.f5.com/images/crdupdater:v0.45.3-0.0.2"
  "repo.f5.com/images/f5-cert-client:v3.6.6"
  "repo.f5.com/images/f5-coremond:v0.16.2"
  "repo.f5.com/images/f5-csm-qkview:v0.14.0"
  "repo.f5.com/images/f5-downloader:v0.32.11-0.0.5"
  "repo.f5.com/images/f5-dssm-store:v5.1.49-0.0.3"
  "repo.f5.com/images/f5-fluentbit:v1.5.2"
  "repo.f5.com/images/f5-fluentd:v2.5.0-0.0.4"
  "repo.f5.com/images/f5ingress:v14.59.1-0.0.70"
  "repo.f5.com/images/f5ing-tmm-pod-manager:v1.6.1-0.0.4"
  "repo.f5.com/images/f5-ipam-controller:v1.5.2-0.0.7"
  "repo.f5.com/images/f5-l4p-engine:v1.130.9-0.0.2"
  "repo.f5.com/images/f5-license-helper:v0.15.1-0.0.2"
  "repo.f5.com/images/f5-lifecycle-operator:v2.21.13-0.0.28"
  "repo.f5.com/images/f5-toda-observer:v5.30.13-0.0.5"
  "repo.f5.com/images/opentelemetry-collector-contrib:0.149.0"
  "repo.f5.com/images/rabbit:v0.6.2"
  "repo.f5.com/images/spk-csrc:v0.9.7-0.0.2"
  "repo.f5.com/images/spk-cwc:v0.41.3-0.0.5"
)

for img in "${BNK_HOST_IMAGES[@]}"; do
  name=$(echo "$img" | sed 's|.*/||; s/:/-/g')
  echo "Pulling $img (amd64) ..."
  skopeo copy --override-arch amd64 docker://$img docker-archive:$HOME/airgap/images/${name}.tar:${img}
done

# DPU images (arm64) — saved as tars for SCP + ctr import (no registry needed for DPU)
mkdir -p ~/shaath/airgap/images-dpu
BNK_DPU_IMAGES=(
  "repo.f5.com/images/tmm-img:v10.159.3-0.1.5"
  "repo.f5.com/images/f5dr-img:v3.28.2"
  "repo.f5.com/images/f5dr-img-init:v3.28.2"
  "repo.f5.com/images/tmrouted-img:v2.20.1-0.0.4"
  "repo.f5.com/images/f5-debug-sidecar:v10.63.4-0.1.5"
  "repo.f5.com/images/f5-blobd:v1.24.4-0.0.3"
  "repo.f5.com/images/f5-coremond:v0.16.2"
  "repo.f5.com/images/f5-fluentbit:v1.5.2"
  "repo.f5.com/images/f5-toda-observer:v5.30.13-0.0.5"
  "repo.f5.com/images/f5-eowyn-install:v0.8.4"
  "repo.f5.com/images/f5-node-labeler:v0.0.27"
)

for img in "${BNK_DPU_IMAGES[@]}"; do
  name=$(echo "$img" | sed 's|.*/||; s/:/-/g')
  echo "Pulling $img (arm64) ..."
  skopeo copy --override-arch arm64 docker://$img docker-archive:$HOME/airgap/images-dpu/${name}.tar:${img}
done
```

### 0.7 Download kubespray binaries and start file server

```bash
mkdir -p ~/shaath/airgap/files

# Binaries kubespray needs (versions pinned to K8s 1.30 + kubespray v2.28.1)
KUBESPRAY_FILES=(
  "https://dl.k8s.io/release/v1.30.14/bin/linux/amd64/kubelet"
  "https://dl.k8s.io/release/v1.30.14/bin/linux/amd64/kubectl"
  "https://dl.k8s.io/release/v1.30.14/bin/linux/amd64/kubeadm"
  "https://github.com/etcd-io/etcd/releases/download/v3.5.22/etcd-v3.5.22-linux-amd64.tar.gz"
  "https://github.com/containernetworking/plugins/releases/download/v1.4.1/cni-plugins-linux-amd64-v1.4.1.tgz"
  "https://github.com/kubernetes-sigs/cri-tools/releases/download/v1.30.1/crictl-v1.30.1-linux-amd64.tar.gz"
  "https://github.com/opencontainers/runc/releases/download/v1.2.6/runc.amd64"
  "https://github.com/containerd/containerd/releases/download/v1.7.23/containerd-1.7.23-linux-amd64.tar.gz"
  "https://github.com/containerd/nerdctl/releases/download/v2.0.5/nerdctl-2.0.5-linux-amd64.tar.gz"
  "https://github.com/projectcalico/calico/releases/download/v3.29.5/calicoctl-linux-amd64"
  "https://github.com/projectcalico/calico/archive/v3.29.5.tar.gz"
)

cd ~/shaath/airgap/files
for url in "${KUBESPRAY_FILES[@]}"; do
  echo "Downloading $url ..."
  curl -fSLO "$url"
done
cd ~

# Create mirrored directory structure for file server
mkdir -p ~/shaath/airgap/fileserver/dl.k8s.io/release/v1.30.14/bin/linux/amd64
mkdir -p ~/shaath/airgap/fileserver/github.com/etcd-io/etcd/releases/download/v3.5.22
mkdir -p ~/shaath/airgap/fileserver/github.com/containernetworking/plugins/releases/download/v1.4.1
mkdir -p ~/shaath/airgap/fileserver/github.com/kubernetes-sigs/cri-tools/releases/download/v1.30.1
mkdir -p ~/shaath/airgap/fileserver/github.com/opencontainers/runc/releases/download/v1.2.6
mkdir -p ~/shaath/airgap/fileserver/github.com/containerd/containerd/releases/download/v1.7.23
mkdir -p ~/shaath/airgap/fileserver/github.com/containerd/nerdctl/releases/download/v2.0.5
mkdir -p ~/shaath/airgap/fileserver/github.com/projectcalico/calico/releases/download/v3.29.5
mkdir -p ~/shaath/airgap/fileserver/github.com/projectcalico/calico/archive

cp ~/shaath/airgap/files/kubelet ~/shaath/airgap/fileserver/dl.k8s.io/release/v1.30.14/bin/linux/amd64/
cp ~/shaath/airgap/files/kubectl ~/shaath/airgap/fileserver/dl.k8s.io/release/v1.30.14/bin/linux/amd64/
cp ~/shaath/airgap/files/kubeadm ~/shaath/airgap/fileserver/dl.k8s.io/release/v1.30.14/bin/linux/amd64/
cp ~/shaath/airgap/files/etcd-v3.5.22-linux-amd64.tar.gz ~/shaath/airgap/fileserver/github.com/etcd-io/etcd/releases/download/v3.5.22/
cp ~/shaath/airgap/files/cni-plugins-linux-amd64-v1.4.1.tgz ~/shaath/airgap/fileserver/github.com/containernetworking/plugins/releases/download/v1.4.1/
cp ~/shaath/airgap/files/crictl-v1.30.1-linux-amd64.tar.gz ~/shaath/airgap/fileserver/github.com/kubernetes-sigs/cri-tools/releases/download/v1.30.1/
cp ~/shaath/airgap/files/runc.amd64 ~/shaath/airgap/fileserver/github.com/opencontainers/runc/releases/download/v1.2.6/
cp ~/shaath/airgap/files/containerd-1.7.23-linux-amd64.tar.gz ~/shaath/airgap/fileserver/github.com/containerd/containerd/releases/download/v1.7.23/
cp ~/shaath/airgap/files/nerdctl-2.0.5-linux-amd64.tar.gz ~/shaath/airgap/fileserver/github.com/containerd/nerdctl/releases/download/v2.0.5/
cp ~/shaath/airgap/files/calicoctl-linux-amd64 ~/shaath/airgap/fileserver/github.com/projectcalico/calico/releases/download/v3.29.5/
cp ~/shaath/airgap/files/v3.29.5.tar.gz ~/shaath/airgap/fileserver/github.com/projectcalico/calico/archive/

# Start nginx file server
docker run -d --name fileserver --restart=always \
  -p 8888:80 \
  -v ~/shaath/airgap/fileserver:/usr/share/nginx/html:ro \
  nginx:stable-alpine
```

### 0.8 Configure kubespray offline vars

```bash
cat > ~/shaath/kubespray/inventory/bnk-airgap/group_vars/all/offline.yml << EOF
---
registry_host: "${JUMPHOST_IP}:5000"
files_repo: "http://${JUMPHOST_IP}:8888"

kube_image_repo: "{{ registry_host }}"
gcr_image_repo: "{{ registry_host }}"
docker_image_repo: "{{ registry_host }}"
quay_image_repo: "{{ registry_host }}"
github_image_repo: "{{ registry_host }}"

github_url: "{{ files_repo }}/github.com"
dl_k8s_io_url: "{{ files_repo }}/dl.k8s.io"
EOF

cat > ~/shaath/kubespray/inventory/bnk-airgap/group_vars/all/containerd.yml << EOF
---
containerd_registries_mirrors:
  - prefix: ${JUMPHOST_IP}:5000
    mirrors:
      - host: https://${JUMPHOST_IP}:5000
        capabilities: ["pull", "resolve"]
        skip_verify: true
EOF
```

### 0.9 Verify Phase 0

```bash
# Check registry
curl -sk https://${JUMPHOST_IP}:5000/v2/_catalog | python3 -m json.tool

# Check file server
curl -s -o /dev/null -w "%{http_code}" http://${JUMPHOST_IP}:8888/dl.k8s.io/release/v1.30.14/bin/linux/amd64/kubeadm
# Expect: 200
```

---

## Phase 4 — Deploy K8s Cluster (kubespray offline)

> This is what dpubnkctl does internally. Shown here for manual testing.

```bash
# Stage SSH key
mkdir -p ~/shaath/kubespray/inventory/bnk-airgap/keys
cp ~/.ssh/id_ed25519 ~/shaath/kubespray/inventory/bnk-airgap/keys/ssh_key
chmod 600 ~/shaath/kubespray/inventory/bnk-airgap/keys/ssh_key

# Run kubespray via Docker (same as dpubnkctl)
docker run --rm -it --network=host \
  -v ~/shaath/kubespray/inventory/bnk-airgap:/inventory \
  -v ~/shaath/kubespray/inventory/bnk-airgap/keys/ssh_key:/root/.ssh/id_rsa:ro \
  quay.io/kubespray/kubespray:v2.28.1 \
  ansible-playbook -i /inventory/hosts.yaml \
  --become --become-user=root \
  --private-key=/root/.ssh/id_rsa \
  -e ansible_user=ubuntu \
  cluster.yml
```

### Verify

```bash
ssh ubuntu@<host-ip> "kubectl get nodes && kubectl get pods -A"
# Expect: all nodes Ready, all pods Running
```

---

## Phase 5 — Join DPUs (offline)

> DPUs don't use the registry. Images are SCP'd as tars and imported with ctr.

```bash
# From host, SCP DPU images + debs to each DPU
scp ~/shaath/airgap/images-dpu/*.tar ubuntu@<dpu-tmfifo-ip>:/tmp/
scp ~/shaath/airgap/containerd-dpu/*.deb ubuntu@<dpu-tmfifo-ip>:/tmp/
scp ~/shaath/airgap/k8s-dpu/*.deb ubuntu@<dpu-tmfifo-ip>:/tmp/

# On DPU: install debs, import images, kubeadm join
# (Same as BNK offline install guide Section 4.1)
```

> **Important:** Wait for SCP to fully complete before running the image import on the DPU.

---

## Phases 7-8 — Deploy BNK (offline)

> Host-side F5 images are imported via ctr (same as offline install guide).
> Helm charts are installed from local tarballs.
> No registry pull needed — images are pre-loaded into containerd.

```bash
# On host: import all F5 BNK host images
for f in ~/shaath/airgap/images/*.tar; do
  sudo ctr -n k8s.io images import "$f"
done

# Install FLO from local chart
helm install f5-lifecycle-operator ~/shaath/airgap/flo/f5-lifecycle-operator-*.tgz \
  --namespace f5-cne-core --create-namespace \
  ...

# Apply cert-manager from local manifest
kubectl apply -f ~/shaath/airgap/cert-manager/cert-manager.yaml

# Continue with CNEInstance, F5SPKVlans, License, etc.
# (Same as BNK offline install guide Sections 6.4-6.11)
```

---

## Lessons Learned (from UDF testing 2026-08-07)

1. **Don't manually curate image lists** — use kubespray's `generate_list.sh` with the correct inventory to resolve exact versions. Manual lists miss dependencies (crictl version mismatch, missing calicoctl, missing pause:3.9, missing dns-node-cache).

2. **Strip registry prefix when pushing to local registry** — kubespray rewrites `quay.io` to `10.1.1.4:5000`, so `quay.io/calico/node` becomes `10.1.1.4:5000/calico/node`. Push images WITHOUT the source registry prefix.

3. **File server must mirror original URL structure** — kubespray constructs download URLs like `files_repo/github.com/containernetworking/...`. The directory tree must match.

4. **containerd_registries_mirrors must include the local registry itself** — not just the upstream prefixes (registry.k8s.io, quay.io). Since image refs are rewritten to point directly at `10.1.1.4:5000`, containerd needs a hosts.toml for that address with `skip_verify: true`.

5. **kube_version must NOT have a `v` prefix** in kubespray — use `1.30.14` not `v1.30.14`.

6. **crictl version maps to K8s major version** — K8s 1.30 → crictl 1.30.1 (not 1.32.0). Always resolve via kubespray's `crictl_supported_versions`.

7. **DPU images bypass the registry entirely** — SCP + `ctr images import` is simpler and proven. No need to configure registry access on DPUs over tmfifo.

---

## Version Reference (kubespray v2.28.1 + K8s 1.30)

| Component | Version | Source |
|---|---|---|
| K8s | 1.30.14 | dpubnkctl version.go |
| etcd | 3.5.22 | kubespray default for K8s 1.30 |
| Calico | 3.29.5 | kubespray default |
| CNI plugins | 1.4.1 | kubespray default |
| crictl | 1.30.1 | kubespray default for K8s 1.30 |
| runc | 1.2.6 | kubespray default |
| containerd | 1.7.23 | dpubnkctl version.go |
| CoreDNS | 1.11.3 | kubespray default for K8s ≥1.30.5 |
| nerdctl | 2.0.5 | kubespray default |
| pause | 3.9 + 3.10 | kubespray needs both |

---

## dpubnkctl Integration Plan

### New CLI flag

```
dpubnkctl e2e --airgap online --yolo   # jumphost has internet → download + serve + deploy
dpubnkctl e2e --airgap offline --yolo  # jumphost has no internet → load from local dir + serve + deploy
dpubnkctl e2e --yolo                   # no airgap — current behavior
```

### Code changes needed

| File | Change |
|---|---|
| `internal/poc/schema.go` | Add `Offline` struct to PoC schema |
| `internal/cli/root.go` | Add `--airgap` flag |
| `internal/cli/e2e.go` | Add Phase 0 before existing phases when `--airgap` |
| `internal/cluster/inventory.go` | Inject offline.yml + containerd.yml into rendered inventory |
| `internal/cluster/join.go` | `dpkg -i` instead of `apt-get install` when offline |
| `internal/deploy/flo.go` | Local helm chart + local cert-manager manifest when offline |
| `internal/deploy/cne.go` | `ctr images import` before applying CRs when offline |
| `internal/version/version.go` | Add kubespray offline image/file lists |
| NEW `internal/airgap/` | Phase 0 logic: registry, file server, TLS, download/load |
