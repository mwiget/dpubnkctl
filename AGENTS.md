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

    Now handled automatically: `deploy_network.go` does a
    `kubectl rollout restart ds/kube-multus-ds` at the tail of the
    phase. Every multus pod re-runs setup with calico already present.
    Cheap (~30s) and idempotent.

    Found on the 2.3 homelab e2e (this branch); same race exists on
    2.2 but bit less often.

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
