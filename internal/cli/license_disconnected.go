package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/mwiget/dpubnkctl/internal/airgap"
	"github.com/mwiget/dpubnkctl/internal/deploy"
	"github.com/mwiget/dpubnkctl/internal/poc"
	"github.com/mwiget/dpubnkctl/internal/ssh"
)

// ErrLicenseManualStepRequired signals that the disconnected license
// flow paused because the jumphost has no internet (offline mode). The
// operator must carry the config report to an internet-connected
// machine, POST it to F5, bring back the manifest, and run
// `dpubnkctl license apply-receipt`.
var ErrLicenseManualStepRequired = fmt.Errorf("license requires manual step (offline mode)")

// runDisconnectedLicense automates the disconnected license flow from
// the offline guide (Section 6.10). In airgap mode the K8s cluster
// cannot reach F5 licensing servers, so the license stays at
// PendingVerification.
//
// Online mode: bounces the config report through the jumphost (which
// has internet) to complete the licensing handshake — fully automated.
//
// Offline mode: extracts the config report and saves it locally, then
// returns ErrLicenseManualStepRequired. The operator carries the file
// to an internet-connected machine and runs `dpubnkctl license
// apply-receipt` to finish.
func runDisconnectedLicense(ctx context.Context, repo string, p *poc.PoC, airgapMode string, r *deploy.Runner, out io.Writer) error {
	if len(p.Hosts) == 0 {
		return fmt.Errorf("no hosts in poc.yaml")
	}
	host := &p.Hosts[0]

	// SSH to the control-plane host.
	hostCfg, err := sshConfigForHost(repo, host, 30*time.Second)
	if err != nil {
		return fmt.Errorf("ssh config for %s: %w", host.Name, err)
	}
	c, err := ssh.Dial(ctx, hostCfg)
	if err != nil {
		return fmt.Errorf("ssh dial %s: %w", host.Name, err)
	}
	defer c.Close()

	// Step 1: Extract CWC client certs.
	fmt.Fprintln(out, "      [license] Extracting CWC client certs ...")
	if err := extractCWCCerts(ctx, c); err != nil {
		return err
	}

	// Step 2: Get CWC auth token.
	fmt.Fprintln(out, "      [license] Getting CWC auth token ...")
	cwcToken, err := getCWCToken(ctx, c)
	if err != nil {
		return err
	}

	// Step 3: Start port-forward in a background goroutine.
	fmt.Fprintln(out, "      [license] Starting CWC port-forward ...")
	pfCtx, pfCancel := context.WithCancel(ctx)
	defer pfCancel()

	pfClient, err := ssh.Dial(pfCtx, hostCfg)
	if err != nil {
		return fmt.Errorf("ssh dial for port-forward: %w", err)
	}
	defer pfClient.Close()

	// Kill any lingering port-forward from a prior run.
	c.Run(ctx, "pkill -f 'port-forward svc/f5-spk-cwc' || true")
	time.Sleep(2 * time.Second)

	pfReady := make(chan struct{}, 1)
	go func() {
		pfReady <- struct{}{}
		pfClient.RunStream(pfCtx,
			"kubectl port-forward svc/f5-spk-cwc 38081:38081 -n f5-cne-core",
			io.Discard)
	}()
	<-pfReady

	// Step 4: Wait for port-forward to be ready (retry /status).
	fmt.Fprintln(out, "      [license] Waiting for CWC port-forward ...")
	statusJSON, err := waitForCWCStatus(ctx, c, cwcToken, out)
	if err != nil {
		return err
	}

	// Step 5: Extract DigitalAssetID.
	fmt.Fprintln(out, "      [license] Extracting DigitalAssetID ...")
	assetID := extractDigitalAssetID(statusJSON)
	if assetID == "" {
		return fmt.Errorf("DigitalAssetID not found in CWC /status response — license may not have reached PendingVerification.\nResponse: %s", statusJSON)
	}
	fmt.Fprintf(out, "      [license] DigitalAssetID: %s\n", assetID)

	// Step 6: Download config report from CWC /report.
	fmt.Fprintln(out, "      [license] Downloading config report from CWC ...")
	if err := downloadConfigReport(ctx, c, cwcToken); err != nil {
		return err
	}

	// Step 7: Pull config report from host to jumphost.
	fmt.Fprintln(out, "      [license] Pulling config report to jumphost ...")
	reportData, err := pullConfigReport(ctx, c)
	if err != nil {
		return err
	}

	// Read JWT (needed for both online POST and offline instructions).
	jwtPath := resolveRef(repo, p.BNK.JWTRef)
	jwt, err := readJWT(jwtPath)
	if err != nil {
		return fmt.Errorf("read JWT for licensing: %w", err)
	}

	// ── Branch: online vs offline ──────────────────────────────────
	if airgapMode == airgap.ModeOffline {
		return handleOfflineLicenseStop(repo, assetID, jwt, reportData, out)
	}

	// Online mode: POST config report to F5 licensing server from jumphost.
	return handleOnlineLicensePost(ctx, c, r, repo, assetID, jwt, reportData, pfCancel, out)
}

// handleOfflineLicenseStop saves the config report and asset ID to the
// PoC artifacts directory and prints instructions for the operator to
// complete licensing from an internet-connected machine.
func handleOfflineLicenseStop(repo, assetID, jwt string, reportData []byte, out io.Writer) error {
	artifactsDir := filepath.Join(repo, "artifacts")
	if err := os.MkdirAll(artifactsDir, 0o755); err != nil {
		return err
	}

	reportPath := filepath.Join(artifactsDir, "license-config-report.json")
	if err := os.WriteFile(reportPath, reportData, 0o600); err != nil {
		return fmt.Errorf("write config report: %w", err)
	}

	assetPath := filepath.Join(artifactsDir, "license-asset-id.txt")
	if err := os.WriteFile(assetPath, []byte(assetID), 0o600); err != nil {
		return fmt.Errorf("write asset ID: %w", err)
	}

	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "      ╔══════════════════════════════════════════════════════════════════╗")
	fmt.Fprintln(out, "      ║  OFFLINE MODE — manual licensing step required                  ║")
	fmt.Fprintln(out, "      ╚══════════════════════════════════════════════════════════════════╝")
	fmt.Fprintln(out, "")
	fmt.Fprintf(out, "      Config report saved to: %s\n", reportPath)
	fmt.Fprintf(out, "      DigitalAssetID: %s\n", assetID)
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "      1. Copy the config report to an internet-connected machine.")
	fmt.Fprintln(out, "      2. Run this curl command:")
	fmt.Fprintln(out, "")
	fmt.Fprintf(out, "         curl -sk -X POST https://product.apis.f5.com/ee/v1/entitlements/telemetry \\\n")
	fmt.Fprintf(out, "           -H \"Content-Type: application/json\" \\\n")
	fmt.Fprintf(out, "           -H \"F5-DigitalAssetId: %s\" \\\n", assetID)
	fmt.Fprintf(out, "           -H \"User-Agent: SPK\" \\\n")
	fmt.Fprintf(out, "           -H \"Authorization: Bearer %s\" \\\n", jwt)
	fmt.Fprintf(out, "           -d @%s \\\n", reportPath)
	fmt.Fprintf(out, "           -o license-manifest.json\n")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "      3. Copy license-manifest.json back to the jumphost.")
	fmt.Fprintln(out, "      4. Run:")
	fmt.Fprintln(out, "")
	fmt.Fprintf(out, "         dpubnkctl license apply-receipt --poc %s --manifest <path-to-license-manifest.json>\n", repo)
	fmt.Fprintln(out, "")

	return ErrLicenseManualStepRequired
}

// handleOnlineLicensePost completes the licensing flow when the
// jumphost has internet: POST to F5, push manifest, POST receipt.
func handleOnlineLicensePost(ctx context.Context, c *ssh.Client, r *deploy.Runner, repo, assetID, jwt string, reportData []byte, pfCancel context.CancelFunc, out io.Writer) error {
	localReport := "/tmp/config-report.json"
	if err := os.WriteFile(localReport, reportData, 0o600); err != nil {
		return fmt.Errorf("write local config-report.json: %w", err)
	}

	// POST config report to F5 licensing server.
	fmt.Fprintln(out, "      [license] POSTing config report to F5 licensing server ...")
	localManifest := "/tmp/license-manifest.json"
	curlArgs := []string{
		"-sk", "-X", "POST",
		"https://product.apis.f5.com/ee/v1/entitlements/telemetry",
		"-H", "Content-Type: application/json",
		"-H", "F5-DigitalAssetId: " + assetID,
		"-H", "User-Agent: SPK",
		"-H", "Authorization: Bearer " + jwt,
		"-d", "@" + localReport,
		"-o", localManifest,
	}
	curlCmd := exec.CommandContext(ctx, "curl", curlArgs...)
	curlOut, err := curlCmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("F5 licensing POST failed: %w\nOutput: %s", err, string(curlOut))
	}
	mfStat, err := os.Stat(localManifest)
	if err != nil || mfStat.Size() < 100 {
		size := int64(0)
		if mfStat != nil {
			size = mfStat.Size()
		}
		return fmt.Errorf("F5 licensing server returned empty/invalid response (%d bytes) — check JWT validity and internet connectivity on jumphost", size)
	}
	fmt.Fprintf(out, "      [license] license-manifest.json received (%d bytes)\n", mfStat.Size())

	// Push manifest to host and POST receipt.
	mfData, err := os.ReadFile(localManifest)
	if err != nil {
		return fmt.Errorf("read local license-manifest.json: %w", err)
	}
	if err := postReceiptAndWait(ctx, c, r, mfData, pfCancel, out); err != nil {
		return err
	}
	return nil
}

// postReceiptAndWait pushes the license manifest to the host, POSTs
// it to CWC /receipt (ONE SHOT), kills the port-forward, and waits
// for the license to reach Active. Shared by both the online flow
// and the `license apply-receipt` command.
func postReceiptAndWait(ctx context.Context, c *ssh.Client, r *deploy.Runner, manifestData []byte, pfCancel context.CancelFunc, out io.Writer) error {
	// Push manifest to host via SFTP.
	fmt.Fprintln(out, "      [license] Pushing license manifest to host ...")
	if err := c.PushBytes(ctx, manifestData, "/tmp/license-manifest.json"); err != nil {
		return fmt.Errorf("SFTP push license-manifest.json: %w", err)
	}

	// POST receipt to CWC — ONE SHOT, do not retry.
	fmt.Fprintln(out, "      [license] POSTing receipt to CWC (ONE SHOT) ...")
	receiptCurl := `curl -sk -X POST ` +
		`--cert /tmp/cwc-client.crt --key /tmp/cwc-client.key --cacert /tmp/cwc-ca.crt ` +
		`-H "Authorization: Bearer $(kubectl get secret cwc-auth-token -n f5-cne-core -o jsonpath='{.data.token}' | base64 -d)" ` +
		`-H "Content-Type: application/json" ` +
		`https://localhost:38081/receipt ` +
		`-d "$(cat /tmp/license-manifest.json | jq -r .manifest)"`
	receiptRes := c.Run(ctx, receiptCurl)
	if !receiptRes.OK() {
		return fmt.Errorf("CWC /receipt POST failed (ONE SHOT — do not retry, manual intervention required): exit %d: %s\nstdout: %s",
			receiptRes.ExitCode, strings.TrimSpace(receiptRes.Stderr), strings.TrimSpace(receiptRes.Stdout))
	}
	fmt.Fprintf(out, "      [license] /receipt response: %s\n", strings.TrimSpace(receiptRes.Stdout))

	// Cancel port-forward.
	if pfCancel != nil {
		pfCancel()
	}

	// Wait for license to reach Active.
	fmt.Fprintln(out, "      [license] Waiting for license state=Active ...")
	if err := deploy.WaitForLicenseActive(ctx, r,
		deploy.LicenseCRName, deploy.SharedComponentNamespace,
		5*time.Minute); err != nil {
		return fmt.Errorf("license did not reach Active after receipt POST: %w", err)
	}
	return nil
}

// ── Shared helpers ─────────────────────────────────────────────────

func extractCWCCerts(ctx context.Context, c *ssh.Client) error {
	certCmds := []struct {
		field  string
		remote string
	}{
		{"ca-root-cert", "/tmp/cwc-ca.crt"},
		{"client-cert", "/tmp/cwc-client.crt"},
		{"client-key", "/tmp/cwc-client.key"},
	}
	for _, cc := range certCmds {
		cmd := fmt.Sprintf(
			`kubectl get secret cwc-license-client-certs -n f5-cne-core -o jsonpath='{.data.%s}' | base64 -d > %s`,
			cc.field, cc.remote)
		res := c.Run(ctx, cmd)
		if !res.OK() {
			return fmt.Errorf("extract CWC cert %s: exit %d: %s — check cwc-license-client-certs secret in f5-cne-core",
				cc.field, res.ExitCode, strings.TrimSpace(res.Stderr))
		}
		check := c.Run(ctx, fmt.Sprintf(`test -s %s`, cc.remote))
		if !check.OK() {
			return fmt.Errorf("CWC cert %s is empty at %s — check cwc-license-client-certs secret in f5-cne-core",
				cc.field, cc.remote)
		}
	}
	return nil
}

func getCWCToken(ctx context.Context, c *ssh.Client) (string, error) {
	tokenRes := c.Run(ctx,
		`kubectl get secret cwc-auth-token -n f5-cne-core -o jsonpath='{.data.token}' | base64 -d`)
	if !tokenRes.OK() {
		return "", fmt.Errorf("get CWC auth token: exit %d: %s — check cwc-auth-token secret in f5-cne-core",
			tokenRes.ExitCode, strings.TrimSpace(tokenRes.Stderr))
	}
	token := strings.TrimSpace(tokenRes.Stdout)
	if token == "" {
		return "", fmt.Errorf("CWC auth token is empty — check cwc-auth-token secret in f5-cne-core")
	}
	return token, nil
}

func waitForCWCStatus(ctx context.Context, c *ssh.Client, cwcToken string, out io.Writer) (string, error) {
	cwcCurl := `curl -sk --cert /tmp/cwc-client.crt --key /tmp/cwc-client.key --cacert /tmp/cwc-ca.crt ` +
		`-H "Authorization: Bearer ` + cwcToken + `" https://localhost:38081/status`
	for attempt := 1; attempt <= 5; attempt++ {
		time.Sleep(12 * time.Second)
		res := c.Run(ctx, cwcCurl)
		if res.OK() && strings.TrimSpace(res.Stdout) != "" {
			return strings.TrimSpace(res.Stdout), nil
		}
		if attempt == 5 {
			return "", fmt.Errorf("CWC /status not reachable after 60s — port-forward may have failed; check `kubectl -n f5-cne-core get svc f5-spk-cwc`")
		}
		fmt.Fprintf(out, "      [license] port-forward not ready, retrying (%d/6) ...\n", attempt)
	}
	return "", fmt.Errorf("unreachable")
}

func downloadConfigReport(ctx context.Context, c *ssh.Client, cwcToken string) error {
	reportCurl := `curl -sk --cert /tmp/cwc-client.crt --key /tmp/cwc-client.key --cacert /tmp/cwc-ca.crt ` +
		`-H "Authorization: Bearer ` + cwcToken + `" https://localhost:38081/report -o /tmp/config-report.json`
	res := c.Run(ctx, reportCurl)
	if !res.OK() {
		return fmt.Errorf("CWC /report download failed: exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	check := c.Run(ctx, `test -s /tmp/config-report.json`)
	if !check.OK() {
		return fmt.Errorf("config-report.json is empty — CWC /report returned no data")
	}
	return nil
}

func pullConfigReport(ctx context.Context, c *ssh.Client) ([]byte, error) {
	catRes := c.Run(ctx, "cat /tmp/config-report.json")
	if !catRes.OK() {
		return nil, fmt.Errorf("cat config-report.json: exit %d: %s", catRes.ExitCode, strings.TrimSpace(catRes.Stderr))
	}
	return []byte(catRes.Stdout), nil
}

// extractDigitalAssetID pulls the DigitalAssetID from the CWC /status
// JSON response. The field lives at different paths depending on whether
// the license is in initial registration or subsequent verification.
func extractDigitalAssetID(statusJSON string) string {
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(statusJSON), &raw); err != nil {
		return ""
	}
	for _, top := range []string{"InitialRegistrationStatus", "Status"} {
		topObj, ok := raw[top].(map[string]interface{})
		if !ok {
			continue
		}
		ld, ok := topObj["LicenseDetails"].(map[string]interface{})
		if !ok {
			continue
		}
		if id, ok := ld["DigitalAssetID"].(string); ok && id != "" {
			return id
		}
	}
	return ""
}
