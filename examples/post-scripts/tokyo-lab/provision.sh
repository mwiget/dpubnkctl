#!/bin/bash
set -euo pipefail

echo "=== Tokyo lab: configuring OVS bridges on DPU ==="

cd "$DPUBNKCTL_POC_DIR"

# Read DPU password
DPU_PASS=$(cat keys/dpu_password.txt)
HOST_IP="172.28.13.16"
DPU_IP="192.168.100.2"

# Clear old known_hosts entry for DPU (reflash changes host key)
ssh -o StrictHostKeyChecking=no -o BatchMode=yes ubuntu@"$HOST_IP" \
  "ssh-keygen -f /home/ubuntu/.ssh/known_hosts -R $DPU_IP 2>/dev/null || true"

# Run OVS bridge setup on DPU via host hop
ssh -o StrictHostKeyChecking=no -o BatchMode=yes ubuntu@"$HOST_IP" \
  "export SSHPASS='$DPU_PASS'; sshpass -e ssh -o StrictHostKeyChecking=no ubuntu@$DPU_IP 'bash -s'" <<'DPUSCRIPT'
set -euo pipefail

# Remove existing bridges
sudo ovs-vsctl del-br sf-external 2>/dev/null || true
sudo ovs-vsctl del-br sf-internal 2>/dev/null || true

# Create external bridge
sudo ovs-vsctl add-br sf-external
sudo ovs-vsctl add-port sf-external p0
sudo ovs-vsctl add-port sf-external en3f0pf0sf1
sudo ovs-vsctl add-port sf-external pf0hpf

# Create internal bridge
sudo ovs-vsctl add-br sf-internal
sudo ovs-vsctl add-port sf-internal p1
sudo ovs-vsctl add-port sf-internal en3f1pf1sf1
sudo ovs-vsctl add-port sf-internal pf1hpf

# Bring up bridges and assign IPs
sudo ip link set sf-external up
sudo ip link set sf-internal up
sudo ip addr add 192.168.40.5/24 dev sf-external 2>/dev/null || true
sudo ip addr add 192.168.50.5/24 dev sf-internal 2>/dev/null || true

echo "--- OVS bridges ---"
sudo ovs-vsctl show
echo "--- Bridge IPs ---"
ip a | grep sf-
DPUSCRIPT

echo "=== DPU OVS bridges configured ==="
