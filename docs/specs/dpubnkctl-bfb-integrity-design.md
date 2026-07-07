# dpubnkctl feature design — sha256-verified `bfb_on_host` + host-direct BFB fetch

**Repo:** `mwiget/dpubnkctl` (local: `~/dev/sp-pm/bnk/dpubnkctl`, currently on `feat/schema-introspection`).
**New branch:** cut from the latest dpubnkctl mainline (confirm base with the user), e.g. `feat/bfb-integrity-host-fetch`.
**Goal:** make provisioning from a remote / low-bandwidth orchestrator both reliable and integrity-checked.

## Background (what already exists — do NOT rebuild)
- `internal/provision/bfb.go`
  - `EnsureBFB(ctx, cacheDir, imageName, urlOverride, progress)` — downloads to the local cache, then
    `verifyBFBChecksum` against `version.BFBImageSHA256`.
  - `verifyBFBChecksum(path)` — sha256 vs `version.BFBImageSHA256`; **no-op when the pin is empty**.
- `internal/version/version.go` — `BFBImage`, `BFBBaseURL`, **`BFBImageSHA256 = ""` (empty → checks disabled)**.
- `internal/cli/provision_dpu.go`
  - `p.Provisioning.BFBOnHost != ""` → skips `EnsureBFB`; `flashOneJob`/`pushAndFlash` receive `bfbOnHost`.
  - `pushAndFlash(... bfbPath, bfbOnHost ...)`: when `bfbOnHost` set, `client.RemoteStat(bfbOnHost)`
    checks **exists + size > 0 only**, then uses it as `remoteBFB` for bfb-install. **No sha256.**
- `internal/ssh/{client.go,sftp.go}` — `PushFile`, `PushBytes`, `RemoteStat`, and a `RunCommand`-style
  exec (confirm exact name) usable to run `sha256sum` / `curl` on the host.
- `internal/poc/schema.go` — `Provisioning` struct: `BFBURL`, `BFBCacheDir`, `BFBOnHost`.

## Part 1 — Integrity: sha256-verify the host BFB
1. **Populate the pin.** Set `version.BFBImageSHA256` to the real digest of the pinned
   `bf-bundle-3.2.0-113_25.10_ubuntu-24.04_64k_prod.bfb` (get it from Mellanox's published checksum or
   `sha256sum` of a known-good copy). This alone makes the *download* path verify.
2. **Per-PoC override (optional).** Add `Provisioning.BFBSHA256 string` (`yaml:"bfb_sha256,omitempty"`)
   so a PoC can pin a specific image (e.g. a custom/older BFB). Precedence: PoC value > `version.BFBImageSHA256`.
   Empty everywhere → warn (not fail) to preserve current unpinned behaviour.
3. **Verify `bfb_on_host` remotely.** In `pushAndFlash`, after the `RemoteStat`, when a digest is known,
   run `sha256sum <bfbOnHost>` on the host (SSH exec), parse the hex, compare to the expected digest.
   Mismatch → fail fast: `bfb_on_host integrity check failed: got <x>, expected <y>`. Add a
   `--skip-bfb-checksum` escape hatch (mirrors the existing `--skip-*` flags) for the unpinned/dev case.
   Print `[cache] host BFB sha256 OK (<digest[:12]>)` on success. Wrap the remote hash in a context with a
   generous timeout (hashing 1.5 GB on the host is ~seconds–tens of seconds, unlike the WAN push).

## Part 2 — Host-direct BFB fetch (`bfb_fetch: host`)
Automate what the operator does by hand today (curl onto the host), so the BFB never round-trips the runner.
1. **Schema.** Add `Provisioning.BFBFetch string` (`yaml:"bfb_fetch,omitempty"`, values `push` (default) | `host`).
   Optionally a host cache dir `Provisioning.BFBHostCacheDir` (default e.g. `/var/cache/dpubnkctl/bfb`).
   Also expose `--bfb-fetch host|push` on `provision dpu` (flag overrides PoC).
2. **Behaviour when `bfb_fetch: host`:**
   - Compute the target host path = `<BFBHostCacheDir>/<BFBImage>`.
   - If it already exists + sha256 matches → reuse (same as `bfb_on_host`, no fetch).
   - Else SSH-exec a fetch on the host: `curl -fSL --retry 5 -o <path>.part <BFBURL>/<BFBImage> && mv <path>.part <path>`
     (or `wget`); stream progress to the operator if practical. Respect `BFBURL` override.
   - sha256-verify on the host (Part 1 #3). Then proceed exactly as `bfb_on_host` (skip local download + push).
   - Fail fast with a clear message if the host lacks curl/wget or has no route to `BFBBaseURL`.
3. **Interaction:** `bfb_fetch: host` and an explicit `bfb_on_host` are mutually exclusive
   (validate warns/errors); `bfb_fetch: push` = today's behaviour. `BFBURL`/`BFBCacheDir` still apply to `push`.

## Validation (`internal/poc/validate.go`)
- `bfb_fetch` ∈ {"", push, host}; `host` requires a resolvable `BFBURL`/pin.
- `bfb_on_host` + `bfb_fetch: host` both set → error.
- Warn when no sha256 is known for any fetch mode (integrity not enforced).

## Tests
- `internal/provision/bfb_test.go` — extend `verifyBFBChecksum` cases (match / mismatch / unpinned).
- New unit tests for the remote-hash parse + expected-digest precedence (PoC > version > empty).
- CLI: `bfb_fetch` flag/PoC precedence; mutual-exclusion validation.
- Manual E2E: on dpu-server-2, set `bfb_fetch: host` and confirm the host curls + verifies + flashes
  with no runner→host push (this is the exact scenario that blocked the D-032 E2E on 2026-07-07).

## Immediate unblock (independent of this feature — usable NOW with the shipped binary)
Stage the pinned BFB on the host and reuse it, no code change:
```
# on dpu-server-2 (fast pipe to content.mellanox.com):
curl -fSL -o /var/cache/dpubnkctl/bfb/bf-bundle-3.2.0-113_25.10_ubuntu-24.04_64k_prod.bfb \
  https://content.mellanox.com/BlueField/BFBs/Ubuntu24.04/bf-bundle-3.2.0-113_25.10_ubuntu-24.04_64k_prod.bfb
# in poc.yaml:
provisioning:
  bfb_on_host: /var/cache/dpubnkctl/bfb/bf-bundle-3.2.0-113_25.10_ubuntu-24.04_64k_prod.bfb
# then re-run provision dpu — it skips download + push.
```
