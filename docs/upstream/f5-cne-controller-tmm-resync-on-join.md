# f5-cne-controller does not push existing Gateway / HTTPRoute config to a late-joining TMM

**Status:** open, no issue filed yet (this branch's audit pass).
**First observed:** BNK 2.3.0-3.2598.3-0.0.170 / FLO `v2.21.13-0.0.28`,
on a 2-DPU homelab (NVIDIA BlueField-3, DOCA 3.2.0, Ubuntu 24.04,
kernel 6.8.0-1012-bluefield-64k). Two host workers, two DPU workers,
single control plane.
**Author:** Marcel Wiget — homelab e2e of `dpubnkctl` v2.3.0
**Last update:** 2026-05-15

## Summary

When a `f5-tmm` pod becomes Ready *after* a `Gateway` / `HTTPRoute`
pair has already been applied to the cluster, the
`f5-cne-controller` does not push the existing route config to the
new TMM. The late TMM ends up with zero virtual servers programmed.
Because traffic to the Gateway's `spec.addresses[].value` reaches
either DPU's TMM via LACP-hashed external-VLAN traffic, roughly half
of incoming requests land on the un-programmed TMM and get the
BIG-IP "no virtual server matched" fallback response:

```
HTTP/1.0 500 Internal Server Error
Server: BigIP
Content-Length: 0
```

A `kubectl rollout restart deploy/f5-cne-controller` is **not**
sufficient to recover — the controller, on startup, does not re-push
existing Gateway/HTTPRoute CRs to all TMMs. The known recovery is
`kubectl delete` + `kubectl apply` on the Gateway and HTTPRoute,
which generates Update events the controller does honour.

## Reproduction

Cluster shape: any BNK 2.3 deployment with **more than one** `f5-tmm`
pod (i.e. more than one DPU node, or `dpu.enabled: true` plus
multiple DPUs in the inventory).

1. Bring the cluster up with `dpubnkctl e2e --yolo`. CNEInstance
   reaches `Available=True`.
2. Apply a Gateway + HTTPRoute pair:

   ```bash
   dpubnkctl gateway example --smoke-test | kubectl apply -f -
   ```

3. At this point assume **one** of the two TMM pods is somehow
   delayed reaching `Ready` (in the homelab e2e, the most common
   trigger is the multus first-start CNI race; can also happen from
   a TMM crash, a node added post-deploy, or a FLO rolling update).
4. Once the late TMM reaches `READY 6/6` with `2/2` readiness gates
   True, attempt a smoke curl:

   ```bash
   ssh worker1 'curl -H "Host: demo-app.local" http://<gw-vip>/'
   ```

5. Observed: HTTP/1.0 500 with `Server: BigIP` on every request that
   LACP-hashes to the late TMM.

## Cluster-side evidence

`kubectl get gateway demo-gw -o yaml | yq .status` (excerpt):

```yaml
addresses:
  - {type: IPAddress, value: 192.168.40.100}
conditions:
  - {type: Accepted,   status: "True", reason: Accepted}
  - {type: Programmed, status: "True", reason: Programmed}
listeners:
  - name: http
    attachedRoutes: 1
    conditions:
      - {type: Accepted,     status: "True"}
      - {type: Programmed,   status: "True"}
      - {type: ResolvedRefs, status: "True"}
```

`kubectl get httproute demo-gw -o yaml | yq .status.parents[0]`:

```yaml
conditions:
  - {type: Accepted,    status: "True"}
  - {type: ResolvedRefs, status: "True"}
controllerName: f5.com/default-f5-cne-controller
```

Both TMMs report all readiness gates True:

```text
f5-tmm-4z2ff (worker1-bf3, JOINED LATE)  6/6 Running  2/2 gates True
f5-tmm-54g2t (worker2-bf3, JOINED EARLY) 6/6 Running  2/2 gates True
```

f5-cne-controller logs from the *original* `kubectl apply` of the
Gateway / HTTPRoute show normal "Reconcile Gateway" + GenerateAndCompileTrafficPolicy events. After the late TMM
joins, **no** corresponding push event for that TMM is logged. The
controller-restart we tried did not produce one either.

After `kubectl delete + apply` of the Gateway / HTTPRoute, the
controller logs the same Reconcile events again — at which point
both TMMs are now present and both receive the config. The curl
flips to `HTTP/1.1 200 OK` within seconds.

## Expected behavior

Either of:

1. When a new `f5-tmm` pod becomes Ready, the cne-controller's
   TMM-watcher should iterate every existing Gateway/HTTPRoute CR
   the controller owns and push the corresponding virtual-server
   config to that TMM. The current behavior treats TMM-Ready as
   purely informational rather than a config-push trigger.

2. Alternatively, on controller startup (e.g. `kubectl rollout
   restart`), the controller should re-push **every** existing
   Gateway/HTTPRoute to **every** TMM. Today it appears to be event-
   driven only; a startup with steady CR state produces no pushes.

Either change closes the gap.

## Suggested fix path

A) **TMM-join watcher in cne-controller**. When the controller's pod
informer sees a new `f5-tmm-*` pod transition to Ready, look up all
Gateway / HTTPRoute CRs that the controller has previously
reconciled and re-emit their reconcile events. The
`GenerateAndCompileTrafficPolicy` path is already idempotent — it
short-circuits with "no change" for any TMM that already has the
config, so this is safe to do on a healthy cluster.

B) **Startup re-emit**. On controller startup, after the informer
caches sync, walk every Gateway / HTTPRoute and re-reconcile.
Operators (and dpubnkctl) could then use a controller restart as a
known-good recovery path — currently it isn't.

(A) is the cleaner long-term answer; (B) is a smaller patch that
also resolves the "restart didn't help" surprise.

## dpubnkctl-side workaround

`dpubnkctl gateway resync` (added on this branch) walks every
Gateway in the cluster, captures its live spec, deletes it, sleeps
briefly, and re-applies. The controller re-pushes to every TMM
currently in the cluster. ~10 seconds per Gateway. Operators run
this after any TMM crash, late node-add, or rolling update.

This is a workaround, not a replacement for the upstream fix —
operator-driven, easy to forget, and creates a brief gap where the
Gateway has `Programmed=False`. A controller-side fix is the right
answer.

## Why this matters for agentic deployment flows

The whole point of agent-driven BNK deployment (`dpubnkctl`'s focus)
is that the agent can drive every recoverable failure mode without
human intervention. This is one of the few mode where the agent has
no signal: the cluster looks healthy in every dimension Kubernetes
exposes (Gateway Programmed, HTTPRoute Accepted+ResolvedRefs, TMM
pods Ready), and only end-to-end traffic reveals the problem. An
agent waiting on "deploy completes successfully" gets a green
light, the operator runs the smoke test, and only then does the
gap surface — by which point the agent has handed off.

A controller-side resync trigger would let agents trust the
"deploy complete + CNEInstance Available + Gateway Programmed"
signal without needing a per-deployment HTTP-200 verification step.

## References

- Reproduced on dpubnkctl `feat/bnk-2.3.0` branch e2e — see
  `journal/2026-05-15-*.md` in the `homelab-2-3-0` PoC
- Workaround commit: see `internal/cli/gateway.go::resync` on `main`
- AGENTS.md gotcha #28 in the dpubnkctl repo
