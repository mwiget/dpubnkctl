# Disconnected License Automation for Airgap Mode

## Context

In airgap `online` mode, the jumphost has internet but the K8s cluster doesn't. The license flow requires a manual 8-step process per the offline guide. dpubnkctl currently applies the License CR with `operationMode: connected` (default) and waits for Active — which never happens in airgap because the cluster can't reach F5 licensing servers.

The fix: when airgap is active, automatically set `operationMode: disconnected` and run the disconnected licensing flow. The jumphost has internet, so all steps can be automated.

## Changes

### 1. Auto-set disconnected mode when airgap

**File:** `internal/cli/deploy_cne.go` — `applyLicenseCR()`

When `f.airgap != ""`, override `f.licenseMode` to `"disconnected"`. No need for the user to pass `--license-mode disconnected` manually.

### 2. Add disconnected license flow after PendingVerification

**File:** `internal/cli/deploy_cne.go` — `applyLicenseCR()`

After the license reaches PendingVerification (currently returns nil with a WARN), run the automated flow instead of returning:

1. SSH to the control-plane host (using existing `sshConfigForHost`)
2. Extract CWC client certs:
   ```
   kubectl get secret cwc-license-client-certs -n f5-cne-core -o jsonpath='{.data.ca-root-cert}' | base64 -d > /tmp/cwc-ca.crt
   kubectl get secret cwc-license-client-certs -n f5-cne-core -o jsonpath='{.data.client-cert}' | base64 -d > /tmp/cwc-client.crt
   kubectl get secret cwc-license-client-certs -n f5-cne-core -o jsonpath='{.data.client-key}' | base64 -d > /tmp/cwc-client.key
   ```
3. Get CWC auth token:
   ```
   kubectl get secret cwc-auth-token -n f5-cne-core -o jsonpath='{.data.token}' | base64 -d
   ```
4. Start port-forward (background):
   ```
   kubectl port-forward svc/f5-spk-cwc 38081:38081 -n f5-cne-core &
   ```
5. Get Digital Asset ID from CWC /status:
   ```
   curl -sk --cert /tmp/cwc-client.crt --key /tmp/cwc-client.key --cacert /tmp/cwc-ca.crt \
     -H "Authorization: Bearer $TOKEN" https://localhost:38081/status
   ```
6. Download config report from CWC /report:
   ```
   curl -sk --cert /tmp/cwc-client.crt --key /tmp/cwc-client.key --cacert /tmp/cwc-ca.crt \
     -H "Authorization: Bearer $TOKEN" https://localhost:38081/report -o /tmp/config-report.json
   ```
7. SCP config-report.json from host to jumphost
8. POST to F5 licensing server FROM THE JUMPHOST (has internet):
   ```
   curl -sk -X POST https://product.apis.f5.com/ee/v1/entitlements/telemetry \
     -H "Content-Type: application/json" \
     -H "F5-DigitalAssetId: $ASSET_ID" \
     -H "User-Agent: SPK" \
     -H "Authorization: Bearer $JWT" \
     -d @config-report.json \
     -o license-manifest.json
   ```
9. SCP license-manifest.json from jumphost back to host
10. POST manifest to CWC /receipt on host:
    ```
    curl -sk -X POST --cert /tmp/cwc-client.crt --key /tmp/cwc-client.key --cacert /tmp/cwc-ca.crt \
      -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
      https://localhost:38081/receipt \
      -d "$(jq -r .manifest /tmp/license-manifest.json)"
    ```
11. Kill port-forward
12. Wait for license state=Active

### 4. Verification checks at each step

Every step must verify success before proceeding:

| Step | Check | Fail action |
|------|-------|-------------|
| CWC certs extraction | All 3 files exist and non-empty | Error: "CWC client certs not found — check cwc-license-client-certs secret in f5-cne-core" |
| CWC auth token | Token string non-empty | Error: "CWC auth token not found — check cwc-auth-token secret in f5-cne-core" |
| Port-forward | curl to localhost:38081 responds within 10s | Retry up to 3 times with 5s wait |
| /status response | JSON contains DigitalAssetID (non-empty) | Error: "CWC /status did not return DigitalAssetID — license may not have reached PendingVerification" |
| /report download | config-report.json exists AND size > 0 bytes | Error: "Config report download failed or empty — CWC /report returned no data" |
| F5 licensing POST | license-manifest.json exists AND size > 100 bytes | Error: "F5 licensing server returned empty/invalid response — check JWT validity and internet connectivity" |
| /receipt POST | HTTP response not error | Error: "Receipt POST failed — this is ONE SHOT, do not retry. Manual intervention required." |
| License state | Reaches Active within 5 minutes | Error with current state + troubleshooting guidance |

### 3. Implementation approach

Steps 2-6 and 10-11 run on the host via SSH (using existing `sshConfigForHost` + `ssh.Dial` + `Run`).

Steps 8-9 run on the jumphost (local exec).

Step 7 uses SCP via SSH (read file from host).

The whole flow is a single function `runDisconnectedLicense()` called from `applyLicenseCR()` when the license reaches PendingVerification and airgap is active.

### Files to modify

1. `internal/cli/deploy_cne.go` — auto-set disconnected mode, call `runDisconnectedLicense()` after PendingVerification
2. No new files needed — all logic in deploy_cne.go using existing SSH utilities

### Verification

1. `go build ./cmd/dpubnkctl/` compiles
2. `go test ./...` passes
3. Full e2e with `--airgap online` — license reaches Active automatically
