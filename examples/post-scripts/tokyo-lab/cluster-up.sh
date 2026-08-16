#!/bin/bash
set -euo pipefail

echo "=== Tokyo lab: setting up kubeconfig on host ==="

HOST_IP="172.28.13.16"

ssh -o StrictHostKeyChecking=no -o BatchMode=yes ubuntu@"$HOST_IP" 'bash -s' <<'HOSTSCRIPT'
set -euo pipefail

if [ ! -f /etc/kubernetes/admin.conf ]; then
  echo "ERROR: /etc/kubernetes/admin.conf not found — cluster-up may have failed"
  exit 1
fi

mkdir -p $HOME/.kube
sudo cp /etc/kubernetes/admin.conf $HOME/.kube/config
sudo chown ubuntu:ubuntu $HOME/.kube/config

echo "--- kubeconfig ready ---"
kubectl get nodes
HOSTSCRIPT

echo "=== Kubeconfig configured ==="
