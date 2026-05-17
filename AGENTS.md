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

If the developer host is an Apple Silicon Mac, `make build` produces a
darwin/arm64 binary that won't execute inside the Linux/arm64 sandbox
where Claude Code agents run (Docker Desktop's linuxkit VM cannot exec
Mach-O). Run `make build-all` to additionally produce
`bin/dpubnkctl-linux-arm64`, which the agent can invoke for
`validate` / `samples` / `init` against the working tree. Both
binaries share the same source and ldflags — only `GOOS` differs.

---

## Required gates on every destructive command

`--yolo` and `--confirm-cluster <NAME>` (matching `poc.yaml.metadata.name`). Pattern is identical across `cluster up`, `cluster reset`, `cluster join-dpus`, `host network setup`, `deploy network/flo/cne`, and `destroy bnk/dpus/`. **Never** add a destructive command without these gates.

The one deliberate exception is `gateway resync` — its blast radius is bounded (a few seconds of Programmed=False per Gateway, no cluster-state loss) and it's expected to run multiple times per cluster lifecycle. Its long help calls out the exemption explicitly; if you add a similar Day-2 tool with bounded impact, document the exemption the same way.

## SSH trust model

dpubnkctl's SSH transport (`internal/ssh`) is TOFU-based per-PoC: `inventory/known_hosts` is created at 0600 on first connect, and every subsequent dial verifies against it. Concurrent TOFU-adds (cluster up fans out in parallel) are serialised by `knownHostsMu` so two goroutines can't race the O_APPEND write.

DPU connections via ProxyJump deliberately skip per-DPU known_hosts because every DPU in every PoC answers at the same tmfifo address (192.168.100.2) — a shared known_hosts would collide the moment a second host's DPU connected. The trust boundary is the jumphost: its key IS pinned in `inventory/known_hosts`, and the DPU is only reachable through that already-verified jumphost. See `internal/cli/ssh_dpu.go::dpuSSHConfig` for the implementation.

---

## Recurring failure modes (gotchas that happen on every PoC)

### Cluster bring-up

1. **kubespray preinstall checks the `ip:` var actually exists on the host** (`Stop if ip var does not match local ips`). When `network.node_ip_role` is set, the host needs the matching VLAN sub-interface UP *before* `cluster reset` OR `cluster up` runs. Order: `host network setup` → `cluster reset/up`. Both playbooks share the preinstall role — reset fails the same way if the IP isn't there.

2. **Don't set kubespray's `access_ip`** — kubespray uses it to override the *advertised* etcd/apiserver endpoint, not to give ansible a different reachability address. With `ip: <data-plane>` and `access_ip: <mgmt>`, etcd binds to data-plane but advertises mgmt → healthcheck dials mgmt → connection refused. `ansible_host` alone is the right knob for SSH reachability. (Fixed in `7d0e9d6`.)

3. **`kubeadm join` does not accept `--node-ip`** as a top-level flag — it's a kubelet flag. To force a non-default kubelet `--node-ip`, write `KUBELET_EXTRA_ARGS=--node-ip=<ip>` to `/etc/default/kubelet` *before* kubeadm join runs (kubeadm starts kubelet via systemd, which sources that file). Same pattern works on hosts and DPUs. (See `internal/cluster/join.go::JoinDPU`.)

4. **kubespray's localhost-nginx-proxy convention breaks externally-joined DPUs.** Default kubespray makes every worker run a localhost nginx-proxy that fans out to control planes; DPUs joined externally (not via kubespray) don't have it. Set `loadbalancer_apiserver_localhost: false` + `loadbalancer_apiserver: {address, port: 6443}` + `apiserver_loadbalancer_domain_name: <addr>` + `supplementary_addresses_in_ssl_keys: [<addr>]` so all nodes share the same routable apiserver address. Driven from `network.cluster_apiserver_address` in poc.yaml.

5. **containerd CRI caches "no CNI" state.** When `install-cni-plugins` lands the CNI binaries + configs *after* containerd has already declared `NetworkPluginNotReady`, containerd doesn't re-scan `/etc/cni/net.d` automatically — node stays `NotReady` until containerd is restarted. Now handled automatically: `cluster_up.go::restartContainerdOnHosts` runs at the tail of `cluster up`, and `deploy_network.go::restartContainerdEverywhere` (hosts + DPUs) runs after the multus/sriov DaemonSets land. If you still see `NetworkPluginNotReady`, run `sudo systemctl restart containerd` on the offending node.

6. **Kubeconfig localization rewrites server URL + drops CA data + adds insecure-skip-tls-verify.** `cluster.LocalizeKubeconfig` handles all three so the kubeconfig that lands at `artifacts/kubeconfig` works from outside the cluster fabric. The apiserver cert SAN doesn't include the mgmt address — kubespray's `supplementary_addresses_in_ssl_keys` knob would extend it but isn't wired through poc.yaml yet, hence the `insecure-skip-tls-verify`. If you need a properly-trusted kubeconfig, add the mgmt address to the kubespray inventory's `supplementary_addresses_in_ssl_keys` before `cluster up`.

### Multus first-start race — broken /etc/cni/net.d/00-multus.conf

26. **Multus pods that boot before calico's `install-cni` initContainer
    finishes write a broken `/etc/cni/net.d/00-multus.conf`** containing
    only the loopback delegate (no calico). Every subsequent pod on
    that node then hangs in `ContainerCreating` with:

      plugin type="multus" name="multus-cni-network" failed (add):
      error adding container to network "": missing network name:

    Cross-check with `cat /etc/cni/net.d/00-multus.conf | jq .delegates`:
    a healthy node has the full calico delegate; a broken node has only
    `[{type: loopback}]`.

    Two-step trap: even after multus rotates, the sriov-cni /
    sriov-device-plugin pods that were stuck `ContainerCreating`
    against the broken multus don't auto-recover — kubelet CRI
    sandbox-creation backoff has grown exponentially and there's no
    event that forces a retry.

    Now **detect-and-fix** in `deploy_network.go` (no per-deploy tax
    on healthy clusters):

      1. Probe `kubectl -n kube-system get pods --field-selector=
         status.phase=Pending` after the standard DS rollouts. A
         healthy cluster returns empty — done. A race-hit cluster
         returns one or more sriov-cni / sriov-device-plugin pods.
      2. On detection only: rollout-restart `kube-multus-ds`,
         `kube-sriov-cni-ds-amd64`, `kube-sriov-device-plugin-amd64`.
         Wait for all three to converge.
      3. Sweep any pod still in `Pending` and `kubectl delete` so
         the kubelet retries against the fresh CNI state.

    Found on the 2.3 homelab e2e (this branch); same race exists on
    2.2 but bites less often.

### f5-cne-controller doesn't re-push Gateways to a late-joining TMM

28. **A TMM pod that joins the cluster after a Gateway has been applied
    ends up with zero virtual servers programmed.** The cne-controller
    pushes TMM config on Gateway/HTTPRoute create/update events, not
    on TMM-pod-Ready events. Symptoms with two DPUs:

      * `kubectl get gateway demo-gw` — `Programmed=True`
      * `kubectl get httproute demo-gw` — `Accepted+ResolvedRefs`
      * Both `f5-tmm` pods `READY 6/6` with `2/2` readiness gates True
      * `curl http://<gw-vip>/` → `HTTP/1.0 500 Server: BigIP`
        (LACP hashes ~50% of flows to the un-programmed TMM)

    Test which TMM is un-programmed (BNK 2.3 path):

      `kubectl -n default logs <tmm-pod> -c f5-tmm | grep -i "virtual"`

    The early TMM logs `virtual server 'default-demo-gw-...' added`;
    the late TMM has no such log line.

    Workarounds, in order of cheapness:
      a) `kubectl delete httproute demo-gw && kubectl delete gateway
         demo-gw && kubectl apply -f <yaml>` — controller re-pushes to
         every TMM currently in the cluster. ~10s.
      b) `dpubnkctl gateway resync` — same idea, walks every Gateway
         in the cluster, delete-and-reapplies each in place. Use after
         any TMM crash / node add / multus race recovery.

    What does NOT help:
      - `kubectl rollout restart deploy/f5-cne-controller` (verified —
        the controller, on startup, doesn't re-push existing CRs to
        TMMs)

    With the multus auto-rotation now in `deploy network` (gotcha #26),
    the most common trigger (a TMM that scheduled late because its
    node's multus was broken) no longer fires. The gotcha can still
    surface from a TMM crash, a node added post-deploy, or a FLO
    rolling-update.

    Upstream issue draft: see `docs/upstream/f5-cne-controller-tmm-resync-on-join.md`.

### Gateway API conformance in 2.3 — HTTPRoute hostnames required

27. **BNK 2.3 HTTPRoutes need `hostnames:` to match traffic.** The 2.3
    release notes call out Gateway API Conformance as an enhancement.
    In practice this means an HTTPRoute with only `matches.path` and
    no `hostnames` list is treated as "no virtual server" by TMM, and
    requests fall through to BIG-IP's fallback:

      HTTP/1.0 500 Internal Server Error
      Server: BigIP

    (BNK 2.2 routed catch-all traffic to such routes; 2.3 doesn't.)

    `dpubnkctl gateway example` now emits `hostnames: ["<app>.local"]`
    by default. Operators curl with `-H "Host: <app>.local"` or set up
    DNS pointing at the Gateway's `spec.addresses` value.

    Found in the Phase 6 e2e smoke test on homelab-2-3-0 (this branch).

### Ghost mlx5_core PF after BFB flash — now auto-recovers

29. **The "ghost PF" remediation has changed: don't reboot.** A successful
    BFB flash leaves the host's mlx5_core PF detached from the kernel
    (sysfs entry exists, `ethtool -i <parent_iface>` returns "No such
    device"). The historical advice was "reboot the host" — but on a
    Proxmox VFIO-passthrough setup that can hang the hypervisor's PCIe
    reset path and take the entire host down. Verified on rome1
    May 15: VMs 204/205 SSH-rebooted simultaneously after the DPU
    reflash, both hung in qemu CPU reset, required a Proxmox-level
    `qm stop` to recover.

    Now handled by two layered fixes:

      1. `provision dpu` waits up to 90s post-SF-ready for the host's
         `parent_iface` to come back live (the BF3 settle window). In
         practice ~10-30s on a healthy run. `--bf3-settle-timeout` /
         `--skip-bf3-settle` override.
      2. `host network setup` detects any remaining ghost state and
         runs `modprobe -r mlx5_core; modprobe mlx5_core` on the host
         to re-probe the PCIe device. Seconds, no reboot, no VFIO
         touch. Only after this fallback fails does it return the
         historical "reboot the host manually" error.

    Net effect: a clean `dpubnkctl e2e` from provision through
    host-network with zero reboots on Proxmox setups.

### Apt repo trap on DOCA 3.2 BFB (kubernetes.sources)

25. **DOCA 3.2 / Ubuntu 24.04 BFBs pre-ship `/etc/apt/sources.list.d/kubernetes.sources`** (deb822
    format) pinned to the *latest* k8s stable (observed: `v1.34`).
    `cluster join-dpus` adds `/etc/apt/sources.list.d/kubernetes.list`
    pointing at the cluster's k8s minor (e.g. `v1.30`), but apt
    *unions* both sources and resolves the highest available version.
    Without removing the pre-shipped deb822 source, the DPU ends up
    with `kubeadm 1.34.x` and refuses to join a 1.30 control plane:

      this version of kubeadm only supports deploying clusters with
      the control plane version >= 1.33.0. Current version: v1.30.14

    Fix (in `internal/cluster/join.go::InstallKubeBinaries`):
    explicitly `rm -f /etc/apt/sources.list.d/kubernetes.sources
    /etc/apt/sources.list.d/kubernetes.list` *before* writing the
    .list. Found in the Phase 6 e2e on homelab-2-3-0 (this branch).

### DPU OS observations on DOCA 3.2.0 / Ubuntu 24.04 (BNK 2.3)

The 2.2 → 2.3 reflash on the homelab (commit history starting at
`0aa147e`) confirmed the existing `bf-lag.conf.tmpl` works as-is on
the new BFB. Two surprises worth knowing:

- **Kernel 6.8 ships both old + new SF interface names side-by-side.**
  `ip link` shows `en3f0pf0sf1` AND `enp3s0f0s1` (likewise on PF1). The
  bf.conf OVS port list still uses the legacy `en3f...` form and works
  unchanged. Operators inspecting `ip link` should not be alarmed by
  the duplicate; they're not separate netdevs, just two stable names
  for the same auxiliary device.

- **NIC firmware ratchet:** the BFB 3.2.0-113 upgrades the BF3 NIC
  firmware to `32.47.1026` automatically during `bfb_post_install`.
  Operators running mixed-version PoCs (some DPUs still on 2.9.2)
  cannot roll back the firmware without explicit
  `mlxfwmanager --downgrade`.

### DPU provisioning + join

7a. **DPU first-boot may come up with no sshd host keys.** The BFB image ships `/var/lib/cloud/instances/nocloud/` pre-stamped from NVIDIA's image build, so cloud-init's `cc_ssh` module sees "Instance link already exists, not recreating it" and skips host-key generation. The fallback (Ubuntu's `ssh-keygen.service` or sshd's internal auto-regen) is racy — sometimes fires, sometimes doesn't, depending on first-boot ordering. When it doesn't, ssh.service restart-loops with `no hostkeys available -- exiting`. Symptom: dpubnkctl provision dpu's `[7/7] Waiting for second DPU boot` times out because sshd never starts. Fixed by `bfb_modify_os` running `chroot /mnt /usr/bin/ssh-keygen -A` so keys are baked into eMMC at flash time (commit `f9d3a59` or similar). Pre-fix DPUs: run `ssh-keygen -A` via rshim serial console (login as ubuntu, password at `keys/dpu_password.txt`).

7. **`mlnx-sf.conf` may silently fail to create one of the SFs at first boot.** The bf.conf-rendered config has both lines, only one runs. Always `sudo mlnx-sf -a show` after first DPU boot to confirm both p0 + p1 SFs exist; recreate manually if not:
   ```
   sudo mlnx-sf --action create --device 0000:03:00.0 --sfnum 1 --enable-trust --hwaddr <mac>
   ```
   Then restart the SR-IOV device plugin pod on the affected node so it re-detects.

8. **OVS internal ports default to MTU 1500** even when `bond0`/`p0`/`p1` are 9000. Result: anything >1500 from the host PCIe path (`pf0hpf`) gets dropped/fragmented inside the DPU OVS, breaking TLS handshakes (apiserver Server Hello + Certificate is multi-KB). bf.conf's `ovs-vlan-init.sh` now sets MTU on `br-lag`/`pf0hpf`/each VLAN port (commit `0815bb0`). For DPUs already deployed without the fix: persistent runtime patch `ovs-vsctl set interface <port> mtu_request=9000` (survives reboot).

9. **BlueField host PF kernel VLAN sub-interfaces work, but may need a fresh boot.** First `ip link add link ens16f0np0 type vlan id N` after a bare-metal boot can fail with `ENODEV` due to stale netlink state from earlier failed configurations. The same netplan that fails to apply pre-reboot will come up cleanly post-reboot. **Don't conclude "BF3 in DPU mode rejects VLAN sub-ifs" without rebooting first.**

10. **DPU kubelet would drift to BSP-pinned 1.30.14** without the `apt-mark unhold` prelude — the BlueField BSP pre-holds kubelet/kubeadm/kubectl, so `apt-get install` is otherwise a no-op against the BSP version. `cluster.InstallKubeBinaries` now runs `apt-mark unhold kubelet kubeadm kubectl || true` before the install, then re-applies `apt-mark hold` to pin the upstream version. If a DPU comes up at the wrong kubelet minor anyway, check the BSP's apt sources first — newer BSP releases may add additional pins.

### Image pull / registry

11. **`cne_pull_64.json` from the FAR tarball is *already base64-encoded***. The `_64` suffix is the tell. The `imagePullSecret` `auth` field for `repo.f5.com` must wrap it as `"_json_key_base64:<contents>"` (NOT re-decoding the contents). `internal/deploy/license.go::buildGARDockerConfig` and `UnwrapGARAuth` round-trip this correctly — bug-for-bug compatible with f5-bnk's auth scheme. If you hit `403 Forbidden — anonymous token` from `repo.f5.com` despite the secret existing, verify the wrapping format first; many similar GAR examples online use the un-suffixed `_json_key:<raw-json>` form which FLO's auth parser rejects.

11a. **`helm registry login` against `repo.f5.com` uses the raw `_json_key:<sa-json>` form (NOT base64).** This is the standard GCP Artifact Registry helm-auth scheme — different from #11, which is the dockerconfigjson the *Kubelet* uses to pull images. Two failure modes worth knowing:

    - Symptom: `Error: authenticating to "repo.f5.com": ... response status code 401: unauthorized: authentication failed` from `helm registry login`. If `helm registry login repo.f5.com -u _json_key --password-stdin <<< <SA-JSON>` works manually with the same FAR tarball, the bug is in how the password is piped, not in the credentials.
    - Specifically: don't use `read -r PW` in a wrapper script — it reads only the first line of stdin, and GCP SA JSONs are pretty-printed multi-line, so the password collapses to literally `{` and the server returns 401. Use `cat | helm registry login --password-stdin` (or pipe the SA JSON directly) so the full body is forwarded. Fixed in `internal/deploy/runner.go::HelmUpgradeOCI`.

12. **`deploy flo` creates `far-secret` in BOTH `f5-operators` AND `default`** (`deploy_flo.go` step 6). The FLO chart references it as `imagePullSecret`, and TMM (which lands in the CNEInstance's namespace — currently `default` — see #17) also needs it. If you still see FLO or TMM pods in `ImagePullBackOff` with `403 Forbidden — anonymous token`, check both namespaces have the secret; the chart's downstream Pod specs reference it by name only.

13. **`deploy flo` installs cert-manager from the pinned upstream URL** as step 4 of its sequence — applies `cert-manager.yaml` from `github.com/cert-manager/cert-manager/releases/download/v1.16.2/`, then waits for all cert-manager deployments to become `Available`. The bnk-ca ClusterIssuer chain is applied as step 5, after the CRDs are present.

14. **`deploy network` pre-creates every namespace referenced by the manifests it applies** (`deploy_network.go::extractNamespaces`). Built-in namespaces (`default`, `kube-system`) are skipped; everything else is `kubectl apply`'d as a Namespace stub first. If you customize the embedded NAD templates to target a different namespace, the pre-create still handles it.

### License JWT

15. **In BNK 2.3, the JWT lives in a `License` CR — NOT in FLO chart values.** The
    license-out-of-flo-values refactor (commit 6a2b9f8) drops the
    `license:` block from `flo-values.yaml.tmpl` entirely. `RenderFLOValues`
    no longer takes a JWT or a jwtType. The CWC reads the TEEM endpoint
    from the JWT's `jku` header at runtime, so prod and tst clusters use
    the same template. The classifier `internal/deploy/license.go::InspectJWT`
    is retained but is now **diagnostic-only** — its output appears in the
    deploy_flo banner so operators can confirm the right JWT was picked
    up, but it doesn't drive any code path. For the BNK 2.2 history of
    how the prod/tst RSA signing keys / x5c chains lived in FLO chart
    values, see `git log --all -- internal/embedded/templates/flo-values-tst.yaml.tmpl`.

15a. **The JWT's `jku` URL is the authoritative signal for prod vs tst** (still true; the CWC uses it). Real-world TST JWTs ship with `iss="F5 Inc."` + `kid="v1"` (the same `iss`/`kid` appear in *both* prod and tst tokens), so substring-matching "tst" in those is useless. What the JWT actually tells you is in `header.jku` — e.g. `https://product-tst.apis.f5networks.net/ee/v1/keys/jwks` for tst, `https://product.apis.f5.com/ee/v1/keys/jwks` for prod. `claims.sub` starting with `TST-` is a strong secondary signal. The classifier keys on `jku` first, `sub` second.

16. **JWT timestamps (`iat`, `f5_sat`) do NOT determine token validity.** Token validity is entirely server-side (revocation lists at the licensing endpoint). Observed example: a JWT with `iat` two years past and `f5_sat` a year past still activates successfully because the server still honors it. Do not debug "expired token" theories based on claim timestamps; the only thing that fails locally is RSA signature verification, which is determined by `jku` (see #15). If signature verification fails, the answer is almost always wrong-template, not expired-token.

### BNK deploy / TMM readiness

17. **TMM lands in the same namespace as the CNEInstance.** FLO's `wholeCluster: true` makes it reconcile any CNEInstance, but the per-tenant workload (TMM, cne-controller, dssm, etc.) is created where the CNEInstance lives. Foundational/shared stack (FLO operator, observer, rabbit, otel-collector, ipam-ctlr, spk-csrc, spk-cwc, crdconversion) stays in `f5-operators`. Templates default to `default` for the CNEInstance + sf NADs (commit `0270d78`).

18. **The FLO chart's `crd-installer` Job auto-creates a CNEInstance in `f5-operators`** as a side effect of `helm install` — even before `dpubnkctl deploy cne` runs. If you deploy CNEInstance to `default` with the namespace fix, you'll have **two** CNEInstances and FLO will reconcile both (TMMs in both namespaces). Always check `kubectl get cneinstance -A` before/after deploys; delete the auto-created one if it's not where you want it.

19. **`deploy cne` step 3 renders + applies the F5SPKVlans** aggregated from every DPU's VLAN block — see `deploy.RenderF5SPKVlans` and `deploy_cne.go:122`. The TMM-side interface name and self-IP land in the rendered `F5SPKVlan` CR. If TMM's `bfd_watcher` still logs `ERROR: vlan name not found` and readiness gates `RoutingDone`/`ConfigurationDone` stay False, verify the CRD installed matches the templates' assumption — see #20 (CRD-name drift).

20. **CRD-name drift between templates and live cluster — audited and fixed.** Earlier the `bnk-gatewayclass.yaml.tmpl` rendered a `BNKGatewayClassConfig` (`gateway.f5.com/v1`) that doesn't exist in BNK 2.2.0. The live CRDs the binary targets are:
   - `F5SPKVlan` (`k8s.f5net.com/v1`) — matches `f5spkvlan.yaml.tmpl`
   - `CNEInstance` (`k8s.f5.com/v1`) — matches `cne-instance.yaml.tmpl`
   - `GatewayClass` (upstream `gateway.networking.k8s.io/v1`) with `controllerName: f5.com/default-f5-cne-controller` — `bnk-gatewayclass.yaml.tmpl` now renders just this (the BNKGatewayClassConfig block was removed)

    `F5BnkGateway` (`k8s.f5net.com/v1`) does exist on the live cluster but is a per-Gateway resource (referenced from a `Gateway`'s `infrastructure.parametersRef`), not part of the GatewayClass. dpubnkctl doesn't create one — operators add it themselves when wiring application Gateways.

    If TMM's bfd_watcher still logs `vlan name not found` after `deploy cne`, check the F5SPKVlans rendered to `artifacts/f5spkvlans-rendered.yaml` against `kubectl get f5spkvlan -A` for name + tag mismatch.

24. **BNK 2.2.0 Gateways require explicit `spec.addresses`.** There is no global IPAM pool in BNK 2.2.0 — applying a `Gateway` without `spec.addresses` leaves it at `Programmed=False, reason=AddressNotAssigned` forever. The f5-cne-controller does create per-Gateway `IPAMRange` CRs named `bnkgw-<ns>-<gatewayName>` for its own bookkeeping, but those are not user-supplied pools — they're internal accounting. (Manually-applied `IPAMRange`s with arbitrary names get rejected with `Failed to extract BnkGateway name`.) Use `dpubnkctl gateway example` to scaffold a Gateway pre-filled with `bnk.external_selfip`; `--smoke-test` adds a backend Deployment + Service for an end-to-end curl path.

30. **`deploy cne` step ordering trap: License CR depends on CNEInstance, not on FLO chart install.** The `licenses.k8s.f5net.com` CRD (and `f5-spk-vlans.k8s.f5net.com`, and the `f5-single-license-quota` ResourceQuota) are installed by FLO's *reconciliation* of a CNEInstance, NOT by `helm install flo`. Confirmed May 16, 2026 on wizard-deploy: post-`deploy flo`, `kubectl get crd | grep k8s.f5net.com` returns empty; the moment CNEInstance is applied, FLO's `crd-installer` Job reconciles and the CRDs appear seconds later. Practical consequence: any attempt to "fix the License race" by applying the License *before* the CNEInstance will time out waiting for the CRD. Correct order in `deploy_cne.go` (verified `e6f768e`): CNEInstance → F5SPKVlans (wait CRD established + admission webhook Ready) → GatewayClass → License (wait Active) → restart f5-tmm DaemonSet → wait CNEInstance Available.

31. **`f5-single-license-quota` "status unknown" race.** When CWC reconciles the License CRD, it creates a `ResourceQuota` named `f5-single-license-quota` in `f5-cne-core` capping `count/licenses.k8s.f5net.com: 1` (product invariant: at most one License CR per cluster). The kube-controller-manager's quota controller populates `status.used` asynchronously — until it does, `kubectl apply` for the License hits `Error from server (Forbidden): licenses.k8s.f5net.com "f5-cne-cluster-license" is forbidden: status unknown for quota: f5-single-license-quota`. This is a fail-closed apiserver behaviour (it won't admit a create against a quota whose current usage is unknown). Self-resolves in seconds-to-minutes. `applyWithQuotaRetry` in `deploy_cne.go` swallows just this error string with a 20×5s budget; any other apply failure surfaces immediately. If the retry budget runs out, point operators at `kubectl describe -n f5-cne-core resourcequota f5-single-license-quota`.

32. **`kubectl rollout restart daemonset/f5-tmm` followed by an immediate CNEInstance-Available wait has a transient window**. The DaemonSet rolling update kills revision-1 pods one at a time and creates revision-2 pods; for a few seconds, the OLD pods are still Ready (gates flipped from before the restart) and NEW pods haven't yet started. `CNEInstance.status.F5TmmAvailable` flips True against the lagging revision-1 pods, `CNEInstance.Available` flips True transiently, and any `kubectl wait` checking Available will return success. Then rolling proceeds, old pods get killed, new pods need ~4 minutes to flip RoutingDone, and CNEInstance flaps back to Available=False. Mitigation: ALWAYS chain `kubectl rollout status daemonset/f5-tmm --timeout=10m` after `kubectl rollout restart` before any Available wait. `rollout status` blocks until every pod on the new revision reports Ready=True — which for TMM means both readiness gates have flipped. Verified May 16 on wizard-deploy: without the status wait, `deploy cne` returned "DONE" with 0-1/2 gates and the cluster took ~4 more minutes to actually settle; with the wait, the cluster is at steady state at exit (`e6f768e`).

### bnk-forge integration (optional Day-2 hook)

29. **`dpubnkctl bnk-forge launch` integrates the PoC with a local
    bnk-forge install** (separate project, currently private at
    https://github.com/sp-prod-field/bnk-forge). Operator-controlled via
    `poc.yaml`:

    ```yaml
    bnk_forge:
      enabled: true
      repo_path: ~/git/bnk-forge
      url: https://localhost
      # admin_username/admin_password optional (default admin/changeme)
    ```

    When `enabled: true`, `cluster up` auto-fires the launch flow at
    the tail of the phase — right after the localized kubeconfig is
    written. This makes the project + cluster appear in bnk-forge's
    UI **before** deploy-network/flo/cne run, so the operator (or a
    troubleshooter) can watch FLO come up, License flip Active, and
    TMM schedule live during the rest of the deployment.

    Policy: dpubnkctl never installs bnk-forge for the operator.
    If the local stack isn't running, the auto-hook skips with an
    info message; the operator brings it up manually (`cd
    ~/git/bnk-forge && make deploy`) and reruns `dpubnkctl bnk-forge
    launch`. Bad credentials surface as a WARN; cluster-up still
    succeeds either way. `--skip-bnk-forge` on `cluster up` bypasses
    the hook for one-off runs.

    Idempotent: project + cluster are ensure-or-skip, keyed by
    `poc.metadata.name`. Safe to call any number of times.

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

## Lab notes

When you exercise a new bare-metal topology, document its quirks here
(non-default switch settings, kernel-renamed PFs, BMC oddities, firmware
versions that needed special handling). Anything that's "obvious in
hindsight but cost a half-day to figure out" belongs here. See
`examples/` for pre-canned `poc.yaml` shapes that mirror common labs.
