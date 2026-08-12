#!/bin/bash
set -euo pipefail

echo "=== Tokyo lab: fixing poc.yaml ==="

cd "$DPUBNKCTL_POC_DIR"

# Remove second DPU block (empty serial, not connected)
sed -i '/^        - serial: ""/,/ip: 192.168.50.6/d' poc.yaml

# Fix DPU lag to false (single DPU, no LAG)
sed -i 's/lag: true/lag: false/g' poc.yaml

# Fix parent interface name
sed -i 's/ens16f0np0/enp13s0f0np0/g' poc.yaml

# Fix expected DPU count
sed -i 's/expected_dpus_per_host: 2/expected_dpus_per_host: 1/' poc.yaml

# Add uplinks to DPU VLANs
sed -i '/ip: 192.168.40.5/a\              uplink: p0' poc.yaml
sed -i '/ip: 192.168.50.5/a\              uplink: p1' poc.yaml

# Fix tabs to spaces
sed -i 's/\t/    /g' poc.yaml

# Fix NFS server
sed -i 's/nfs_path:/nfs_server: 192.168.100.1\n    nfs_path:/' poc.yaml

echo "--- poc.yaml fixes applied ---"
grep -n "serial:\|expected_dpus\|lag:\|uplink:\|parent_iface:" poc.yaml
