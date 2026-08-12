# Pod-to-Service Routing Investigation Plan

## Problem

Pods on the DPU cannot reach Kubernetes service IPs (e.g. 10.233.0.1:443).
From the DPU host network namespace, service IPs work fine.
This blocks f5-node-labeler and any future pod that needs API access from the DPU.

This issue exists in ALL dpubnkctl offline deployments tested. The manual offline guide does NOT have this issue.

## Known Facts

| Test | Result |
|------|--------|
| DPU host → `curl -k https://10.233.0.1:443/healthz` | OK |
| DPU host → `curl -k https://192.168.50.16:6443/healthz` | OK |
| Pod on DPU → 10.233.0.1:443 (nsenter curl) | FAIL (timeout) |
| kube-ipvs0 exists with 10.233.0.1 | Yes |
| kube-proxy mode | ipvs |
| net.ipv4.vs.conntrack | 1 |
| Calico CNI sets up pod correctly (route, veth) | Yes |
| Test pod has default route (169.254.1.1) | Yes |
| tcpdump on DPU showed zero packets from pod | Yes (timing may be off) |
| NetworkPolicy blocking | None |
| Manual offline guide node-labeler | Works |

## Key Differences: dpubnkctl vs Manual Offline Guide

| Setting | dpubnkctl (kubespray) | Manual Offline Guide (kubeadm) |
|---------|----------------------|-------------------------------|
| Pod CIDR | 10.233.64.0/18 | 192.168.0.0/16 |
| Service CIDR | 10.233.0.0/18 | 10.96.0.0/12 |
| Cluster setup | kubespray v2.28.1 | bare kubeadm |
| Calico deploy | tigera-operator | tigera-operator |
| kube-proxy mode | ipvs | ipvs |
| DPU join | custom kubeadm join (dpubnkctl Phase 6) | manual kubeadm join |

## Investigation Plan

### Phase 1: Capture broken state (dpubnkctl deployment — CURRENT)

Run on the DPU (dpu-server-2-bf3) before destroying:

```bash
sudo iptables-save > /tmp/dpubnkctl-iptables.txt
ip route show table all > /tmp/dpubnkctl-routes.txt
ip rule list > /tmp/dpubnkctl-rules.txt
sudo ipvsadm -Ln > /tmp/dpubnkctl-ipvs.txt
sudo sysctl -a 2>/dev/null | grep -E "vs\.|forward|rp_filter|bridge|conntrack" > /tmp/dpubnkctl-sysctl.txt
ip addr show > /tmp/dpubnkctl-addrs.txt
sudo cat /etc/cni/net.d/* > /tmp/dpubnkctl-cni.txt 2>/dev/null
```

Run on dpu-server-2 (host):

```bash
kubectl get configmap kube-proxy -n kube-system -o yaml > /tmp/dpubnkctl-kube-proxy-cm.txt
```

Copy all /tmp/dpubnkctl-*.txt files from DPU to a safe location.

### Phase 2: Destroy dpubnkctl deployment

```bash
# On jumphost
cd ~/shaath
./dpubnkctl-airgap destroy --yolo --confirm-cluster test-airgap4 --poc test-airgap4
```

### Phase 3: Deploy manual offline guide

Follow the manual offline guide on dpu-server-2 + dpu-server-2-bf3.
Wait until ALL pods are Running, including f5-node-labeler (Completed status is OK — it runs, labels, exits).

### Phase 4: Capture working state (manual offline guide)

Run the same commands on the DPU:

```bash
sudo iptables-save > /tmp/offline-iptables.txt
ip route show table all > /tmp/offline-routes.txt
ip rule list > /tmp/offline-rules.txt
sudo ipvsadm -Ln > /tmp/offline-ipvs.txt
sudo sysctl -a 2>/dev/null | grep -E "vs\.|forward|rp_filter|bridge|conntrack" > /tmp/offline-sysctl.txt
ip addr show > /tmp/offline-addrs.txt
sudo cat /etc/cni/net.d/* > /tmp/offline-cni.txt 2>/dev/null
```

Run on dpu-server-2 (host):

```bash
kubectl get configmap kube-proxy -n kube-system -o yaml > /tmp/offline-kube-proxy-cm.txt
```

### Phase 5: Compare

Copy all files to Mac and diff:

```bash
diff /tmp/dpubnkctl-routes.txt /tmp/offline-routes.txt
diff /tmp/dpubnkctl-sysctl.txt /tmp/offline-sysctl.txt
diff /tmp/dpubnkctl-iptables.txt /tmp/offline-iptables.txt
diff /tmp/dpubnkctl-ipvs.txt /tmp/offline-ipvs.txt
diff /tmp/dpubnkctl-cni.txt /tmp/offline-cni.txt
diff /tmp/dpubnkctl-kube-proxy-cm.txt /tmp/offline-kube-proxy-cm.txt
```

The differences tell us exactly what's missing in the dpubnkctl DPU config.

### Phase 6: Fix

Apply the missing configuration to dpubnkctl's Phase 6 (join.go) or Phase 7 (deploy-network). Rebuild binary. Test.

## Notes

- All captures stored on dpu-server-2 at `~/shaath-bnk-install/offline-info/`
- DPU data captured via `sshpass -e ssh 192.168.100.2` from dpu-server-2
- ipvsadm not installed on DPU (not in BFB image) — skip in both captures
- DPU password for current deployment: 5C5BBFMgGmJw (changes per BFB flash)

## Status

- [x] Phase 1: Capture broken state (saved to offline-info/dpubnkctl/)
- [ ] Phase 2: Destroy dpubnkctl
- [ ] Phase 3: Deploy manual offline guide
- [ ] Phase 4: Capture working state (save to offline-info/manual/)
- [ ] Phase 5: Compare
- [ ] Phase 6: Fix
