#!/bin/bash
set -euo pipefail

echo "=== Tokyo lab: configuring host data-plane interfaces ==="

HOST_IP="172.28.13.16"

ssh -o StrictHostKeyChecking=no -o BatchMode=yes ubuntu@"$HOST_IP" 'bash -s' <<'HOSTSCRIPT'
set -euo pipefail

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
sudo netplan apply || echo "WARN: netplan apply returned non-zero (warnings are expected)"

# Remove VLAN sub-interfaces created by dpubnkctl (we use flat PFs)
sudo ip link del external40 2>/dev/null || true
sudo ip link del internal50 2>/dev/null || true

echo "--- Waiting for interfaces to come up ---"
sleep 5

echo "--- Connectivity check ---"
ping -c 2 -W 3 192.168.40.5 || echo "WARN: ping 192.168.40.5 failed (may need more time)"
ping -c 2 -W 3 192.168.50.5 || echo "WARN: ping 192.168.50.5 failed (may need more time)"
HOSTSCRIPT

echo "=== Host data-plane configured ==="
