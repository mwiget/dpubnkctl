# AGENTS.md — dpubnkctl Reference for Coding Agents

> Single-binary Go CLI to deploy F5 BIG-IP Next for Kubernetes (BNK) 2.2.0
> on bare-metal hosts with NVIDIA BlueField-3 DPUs. Phased pipeline:
> `discover → provision → host network setup → cluster up → cluster join-dpus → deploy network → deploy flo → deploy cne`. Symmetric:
> `destroy → bnk + dpus + cluster reset`.

---

## Quick orientation

```
cmd/dpubnkctl/                main()
internal/cli/                 cobra commands (one .go per subcommand)
internal/poc/                 poc.yaml schema (source of truth per PoC)
internal/cluster/             kubespray inventory render + Docker run
internal/deploy/              Helm/kubectl runner + license JWT parsing
internal/provision/           bf.conf rendering for BFB flash
internal/embedded/templates/  Go templates (bf.conf, NADs, CNEInstance, FLO values)
internal/ssh/                 ProxyJump-aware SSH client
```

PoC repo layout (separate from this codebase, lives at `/tmp/<name>/` or wherever the operator picks):
```
poc.yaml                      single source of truth
keys/                         FAR tarball, JWT, SSH keys (gitignored)
artifacts/                    rendered manifests, fetched kubeconfig, logs
journal/<date>-<phase>.md     auto-appended timeline (deploy + destroy)
```

---

## Build / Test

```bash
go build -o /tmp/dpubnkctl-bin ./cmd/dpubnkctl
go test ./...
go vet ./...
```

No fancy tooling — stdlib + cobra + yaml.v3 + golang.org/x/crypto/ssh + sftp.

---

## Required gates on every destructive command

`--yolo` and `--confirm-cluster <NAME>` (matching `poc.yaml.metadata.name`). Pattern is identical across `cluster up`, `cluster reset`, `cluster join-dpus`, `host network setup`, `deploy network/flo/cne`, and `destroy bnk/dpus/`. **Never** add a destructive command without these gates.

---

## Recurring failure modes (gotchas that happen on every PoC)

### Cluster bring-up

1. **kubespray preinstall checks the `ip:` var actually exists on the host** (`Stop if ip var does not match local ips`). When `network.node_ip_role` is set, the host needs the matching VLAN sub-interface UP *before* `cluster reset` OR `cluster up` runs. Order: `host network setup` → `cluster reset/up`. Both playbooks share the preinstall role — reset fails the same way if the IP isn't there.

2. **Don't set kubespray's `access_ip`** — kubespray uses it to override the *advertised* etcd/apiserver endpoint, not to give ansible a different reachability address. With `ip: <data-plane>` and `access_ip: <mgmt>`, etcd binds to data-plane but advertises mgmt → healthcheck dials mgmt → connection refused. `ansible_host` alone is the right knob for SSH reachability. (Fixed in `7d0e9d6`.)

3. **`kubeadm join` does not accept `--node-ip`** as a top-level flag — it's a kubelet flag. To force a non-default kubelet `--node-ip`, write `KUBELET_EXTRA_ARGS=--node-ip=<ip>` to `/etc/default/kubelet` *before* kubeadm join runs (kubeadm starts kubelet via systemd, which sources that file). Same pattern works on hosts and DPUs. (See `internal/cluster/join.go::JoinDPU`.)

4. **kubespray's localhost-nginx-proxy convention breaks externally-joined DPUs.** Default kubespray makes every worker run a localhost nginx-proxy that fans out to control planes; DPUs joined externally (not via kubespray) don't have it. Set `loadbalancer_apiserver_localhost: false` + `loadbalancer_apiserver: {address, port: 6443}` + `apiserver_loadbalancer_domain_name: <addr>` + `supplementary_addresses_in_ssl_keys: [<addr>]` so all nodes share the same routable apiserver address. Driven from `network.cluster_apiserver_address` in poc.yaml.

5. **containerd CRI caches "no CNI" state.** When `install-cni-plugins` lands the CNI binaries + configs *after* containerd has already declared `NetworkPluginNotReady`, containerd doesn't re-scan `/etc/cni/net.d` automatically. Restart containerd (`sudo systemctl restart containerd`) to pick them up — node flips to `Ready` immediately. **Tool gap**: cluster up doesn't do this restart automatically (#68).

6. **kubeconfig localization: server URL stays at the data-plane address.** `cluster.LocalizeKubeconfig` doesn't actually rewrite to the SSH/mgmt address — local kubectl from outside the cluster fabric can't reach data-plane IPs. Workaround: edit `artifacts/kubeconfig` to `server: https://<mgmt>:6443` + `insecure-skip-tls-verify: true` (apiserver cert SAN doesn't include mgmt either, that's a sibling gap).

### DPU provisioning + join

7. **`mlnx-sf.conf` may silently fail to create one of the SFs at first boot** (lake1: worker1-bf3 missing p0 SF). The bf.conf-rendered config has both lines, only one runs. Always `sudo mlnx-sf -a show` after first DPU boot to confirm both p0 + p1 SFs exist; recreate manually if not:
   ```
   sudo mlnx-sf --action create --device 0000:03:00.0 --sfnum 1 --enable-trust --hwaddr <mac>
   ```
   Then restart the SR-IOV device plugin pod on the affected node so it re-detects.

8. **OVS internal ports default to MTU 1500** even when `bond0`/`p0`/`p1` are 9000. Result: anything >1500 from the host PCIe path (`pf0hpf`) gets dropped/fragmented inside the DPU OVS, breaking TLS handshakes (apiserver Server Hello + Certificate is multi-KB). bf.conf's `ovs-vlan-init.sh` now sets MTU on `br-lag`/`pf0hpf`/each VLAN port (commit `0815bb0`). For DPUs already deployed without the fix: persistent runtime patch `ovs-vsctl set interface <port> mtu_request=9000` (survives reboot).

9. **BlueField host PF kernel VLAN sub-interfaces work, but may need a fresh boot.** First `ip link add link ens16f0np0 type vlan id N` after a bare-metal boot can fail with `ENODEV` due to stale netlink state from earlier failed configurations. The same netplan that fails to apply pre-reboot will come up cleanly post-reboot. **Don't conclude "BF3 in DPU mode rejects VLAN sub-ifs" without rebooting first.**

10. **DPU kubelet binary version drifts to BSP-pinned 1.30.14** even when `cluster join-dpus` installs `kubelet:1.32.x` from `pkgs.k8s.io`. Likely cause: the BSP's `apt-mark hold kubelet` survives the install. Fix: `apt-mark unhold kubelet kubeadm kubectl` before the install in `InstallKubeBinaries`. Within k8s skew policy (kubelet up to 3 minor versions older than apiserver) so usually works, but worth tightening (#68).

### Image pull / registry

11. **`cne_pull_64.json` from the FAR tarball is *already base64-encoded***. The `_64` suffix is the tell. To build the imagePullSecret `auth` field for `repo.f5.com`:
    ```
    AUTH=$(echo -n "_json_key_base64:$(cat cne_pull_64.json)" | base64 -w0)
    ```
    The current `internal/deploy/license.go` re-encodes — verify before reuse (#68).

12. **`deploy flo` doesn't create `far-secret`**. Chart references it as `imagePullSecret`, never created → all FLO/TMM pods `ImagePullBackOff` with `403 Forbidden — anonymous token`. Workaround: hand-create the secret in `f5-operators` AND `default` (TMM lives there). Tool gap (#68).

13. **`deploy flo` doesn't install cert-manager.** It applies ClusterIssuer/Certificate that need cert-manager CRDs. Manual prereq:
    ```
    kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.16.2/cert-manager.yaml
    kubectl -n cert-manager wait --for=condition=Available --timeout=3m deploy --all
    ```

14. **`deploy network`'s `nad-sf.yaml` requires the target namespace to pre-exist** (`Error from server (NotFound): namespaces "default" not found` on a truly fresh cluster — though `default` always exists, `f5-operators` does not). For any namespace the NADs reference, create it first (#68).

### License JWT

15. **JWT type detection (`prod` vs `tst`) cannot rely on `iss` or `kid` alone.** Lake1 JWT has `iss="F5 Inc."` + `kid="v1"` (no "tst" anywhere), but the `jku` URL points at `product-tst.apis.f5networks.net` and `sub` starts with `TST-`. Wrong template → wrong RSA modulus baked in → "Unverifiable JWT: token signature is invalid: crypto/rsa". Classifier now checks `header.jku` and `claims.sub` (commit `f8eaf15`).

16. **F5 license activation reports expired tokens as "Unverifiable JWT" / signature errors.** No `exp` claim — the relevant time bound is `f5_sat` in claims (subscription activation). When `f5_sat` is past, activation fails with the misleading signature error. **Verify the JWT is in-date before debugging signature paths**:
    ```
    cat keys/.jwt | cut -d. -f2 | base64 -d | python3 -c "import sys,json,datetime; c=json.load(sys.stdin); ts=c['f5_sat']; print('f5_sat=', datetime.datetime.utcfromtimestamp(ts))"
    ```
    To independently confirm the RSA signature is genuinely fine:
    ```python
    # Fetch JWKS, find the kid, RSA-verify the JWT segments — if VALID,
    # the "signature" error is really an expiry mislabel.
    ```

### BNK deploy / TMM readiness

17. **TMM lands in the same namespace as the CNEInstance.** FLO's `wholeCluster: true` makes it reconcile any CNEInstance, but the per-tenant workload (TMM, cne-controller, dssm, etc.) is created where the CNEInstance lives. Foundational/shared stack (FLO operator, observer, rabbit, otel-collector, ipam-ctlr, spk-csrc, spk-cwc, crdconversion) stays in `f5-operators`. Templates default to `default` for the CNEInstance + sf NADs (commit `0270d78`).

18. **The FLO chart's `crd-installer` Job auto-creates a CNEInstance in `f5-operators`** as a side effect of `helm install` — even before `dpubnkctl deploy cne` runs. If you deploy CNEInstance to `default` with the namespace fix, you'll have **two** CNEInstances and FLO will reconcile both (TMMs in both namespaces). Always check `kubectl get cneinstance -A` before/after deploys; delete the auto-created one if it's not where you want it.

19. **`deploy cne` doesn't apply F5SPKVlan/Gateway resources** — templates exist (`f5spkvlan.yaml.tmpl`, `bnk-gatewayclass.yaml.tmpl`) but are never wired through `RunDeployCNE`. Without F5SPKVlans, TMM's bfd_watcher logs `ERROR: vlan name not found` and readiness gates `RoutingDone`/`ConfigurationDone` stay False forever. Apply manually with the rendered VLAN/SelfIP data from poc.yaml (#68).

20. **CNEInstance CRD names don't match dpubnkctl's templates.** Live cluster has `f5-spk-vlans.k8s.f5net.com` (kind `F5SPKVlan`), `f5-bnkgateways.k8s.f5net.com` (kind `F5BnkGateway`), and standard `gateway.networking.k8s.io/v1` GatewayClass — not the `BNKGatewayClassConfig` our `bnk-gatewayclass.yaml.tmpl` assumes. Verify against the installed CRD before applying (`kubectl get crd | grep f5`).

### Destroy / cleanup

21. **F5 sub-CRs leak finalizers when the parent CNEInstance is deleted.** FLO reliably removes the finalizer on its own CNEInstance, but `csrcs`/`cwcs`/`observers`/`rabbitmqs`/`otelcollectors`/`cnemanifests`/etc. get stuck on Terminating. `destroy bnk` force-deletes them and patches `metadata.finalizers: []`. Without this, namespace deletion hangs forever.

22. **kubespray reset.yml only handles hosts in inventory** — externally-joined DPUs need separate `kubeadm reset` first. `destroy dpus` does this. Order matters: do `destroy bnk` → `destroy dpus` → `cluster reset`. The `destroy` top-level command runs all three.

23. **Lab network: data-plane usually has NO internet, mgmt does.** Image pulls/DNS go through the host's default route (mgmt) — apiserver/kubelet/east-west goes through data-plane. Confirm: TCP connect to repo.f5.com getting `403 Forbidden` (HTTP response) means internet is fine, the issue is auth (missing `far-secret`). Connection timeout would mean the route is broken.

---

## Style + conventions

- **Cobra command per `.go` file** in `internal/cli/`, named `<verb>_<noun>.go` (e.g. `cluster_up.go`, `deploy_flo.go`, `destroy_bnk.go`). Top-level `<verb>.go` assembles the subtree.
- **Schema additions go in `internal/poc/schema.go`** with structured comments explaining every field's purpose and impact (see `Network.NodeIPRole` for an example). Add helper methods (`Host.VLANByRole`, `DPU.PortName()`) when callers need to compute derived values.
- **Templates in `internal/embedded/templates/`** are Go `text/template`. For `<<'EOS'` heredocs inside bf.conf, Go template substitutions happen *before* bash sees the text — so `{{.DPUMtu}}` substitutes to a literal number, while `$MTU` references inside the heredoc body remain bash variables (single-quoted heredoc preserves `$`).
- **Always run `go test ./...` and `go vet ./...` before committing.** Tests are in `internal/cluster/inventory_test.go`, `internal/deploy/cne_test.go`, `internal/provision/render_test.go` — keep them green.
- **Commit messages follow Conventional Commits.** `feat(scope): ...`, `fix(scope): ...`, `chore(scope): ...`. Scope = top-level subcommand or internal package (e.g. `feat(destroy):`, `fix(cluster):`, `fix(provision):`).
- **Document the *why* in commit messages**, not just the *what*. Especially for fixes — a future agent should be able to read the commit and understand the failure mode without re-debugging it.

---

## Tested topologies

- **lake1**: 2 hosts (worker1, worker2) × 1 DPU each (worker1-bf3, worker2-bf3) = 4-node cluster, both hosts as control planes (1 etcd for HA-safety), data-plane VLAN 41 with MTU 9000, mgmt 192.168.68.x. End-to-end deploy through `deploy cne` complete; TMM gates blocked only by expired JWT.

If you add a new lab topology, document its quirks here so future agents know what's normal vs novel.
