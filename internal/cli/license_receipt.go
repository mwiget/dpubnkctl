package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mwiget/dpubnkctl/internal/deploy"
	"github.com/mwiget/dpubnkctl/internal/poc"
	"github.com/mwiget/dpubnkctl/internal/ssh"
)

type licenseReceiptFlags struct {
	pocDir   string
	manifest string
}

func newLicenseCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "license",
		Short: "License management for offline/disconnected deployments",
	}
	cmd.AddCommand(newLicenseApplyReceiptCmd())
	return cmd
}

func newLicenseApplyReceiptCmd() *cobra.Command {
	f := &licenseReceiptFlags{}
	cmd := &cobra.Command{
		Use:   "apply-receipt",
		Short: "Apply a license manifest obtained from an internet-connected machine (offline mode step 2)",
		Long: `Complete the disconnected license flow for offline/air-gapped deployments.

After "dpubnkctl deploy cne --airgap offline" extracts the config report and
stops with instructions, the operator:

  1. Carries config-report.json to an internet-connected machine
  2. Runs the curl command printed by deploy cne to get license-manifest.json
  3. Carries license-manifest.json back to the jumphost
  4. Runs this command to finish licensing

This command SSHes to the control-plane host, pushes the manifest, POSTs it
to CWC's /receipt endpoint (ONE SHOT), and waits for the license to reach Active.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLicenseApplyReceipt(cmd.Context(), cmd.OutOrStdout(), f)
		},
	}
	cmd.Flags().StringVar(&f.pocDir, "poc", "", "PoC repo path (default: current directory)")
	cmd.Flags().StringVar(&f.manifest, "manifest", "", "Path to license-manifest.json (required)")
	_ = cmd.MarkFlagRequired("manifest")
	return cmd
}

func runLicenseApplyReceipt(ctx context.Context, out io.Writer, f *licenseReceiptFlags) error {
	repo, err := resolvePoCDir(f.pocDir)
	if err != nil {
		return err
	}
	p, err := poc.Load(repo)
	if err != nil {
		return fmt.Errorf("not a PoC repo (%s): %w", repo, err)
	}
	if len(p.Hosts) == 0 {
		return fmt.Errorf("no hosts in poc.yaml")
	}

	// Verify the prior flow ran by checking the asset ID file.
	assetPath := filepath.Join(repo, "artifacts", "license-asset-id.txt")
	assetData, err := os.ReadFile(assetPath)
	if err != nil {
		return fmt.Errorf("read %s: %w — run `deploy cne --airgap offline` first to extract the config report", assetPath, err)
	}
	assetID := strings.TrimSpace(string(assetData))
	if assetID == "" {
		return fmt.Errorf("license-asset-id.txt is empty — run `deploy cne --airgap offline` first")
	}
	fmt.Fprintf(out, "DigitalAssetID: %s\n", assetID)

	// Read the manifest file.
	mfData, err := os.ReadFile(f.manifest)
	if err != nil {
		return fmt.Errorf("read manifest %s: %w", f.manifest, err)
	}
	// Validate before we SSH anywhere. The operator hand-carried this file
	// between machines, so a truncated copy or a saved HTTP error page is
	// the likely failure — and the /receipt POST it feeds is a one-shot.
	// postReceiptAndWait re-checks, but failing here keeps a bad file from
	// costing a round trip to the cluster.
	if _, err := licenseManifestPayload(mfData); err != nil {
		return fmt.Errorf("manifest %s is not usable: %w", f.manifest, err)
	}
	fmt.Fprintf(out, "Manifest: %s (%d bytes)\n\n", f.manifest, len(mfData))

	// SSH to the control-plane host.
	host := &p.Hosts[0]
	hostCfg, err := sshConfigForHost(repo, host, 30*time.Second)
	if err != nil {
		return fmt.Errorf("ssh config for %s: %w", host.Name, err)
	}
	c, err := ssh.Dial(ctx, hostCfg)
	if err != nil {
		return fmt.Errorf("ssh dial %s: %w", host.Name, err)
	}
	defer c.Close()

	// Re-extract CWC certs (may have been cleaned from /tmp).
	fmt.Fprintln(out, "[1/4] Extracting CWC client certs ...")
	if err := extractCWCCerts(ctx, c); err != nil {
		return err
	}
	defer cleanupCWCCerts(ctx, c)

	// Start port-forward.
	fmt.Fprintln(out, "[2/4] Starting CWC port-forward ...")
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

	// Wait for port-forward to be ready.
	fmt.Fprintln(out, "      Waiting for port-forward ...")
	cwcToken, err := getCWCToken(ctx, c)
	if err != nil {
		return err
	}
	if _, err := waitForCWCStatus(ctx, c, cwcToken, out); err != nil {
		return err
	}

	// Push manifest and POST receipt.
	fmt.Fprintln(out, "[3/4] Applying license manifest ...")
	kubeconfig, err := requireKubeconfig(repo, "cluster must be running")
	if err != nil {
		return err
	}
	r := &deploy.Runner{KubeconfigPath: kubeconfig, Out: prefixWriter{w: out, prefix: "      | "}}

	if err := postReceiptAndWait(ctx, c, r, mfData, pfCancel, out); err != nil {
		return err
	}

	fmt.Fprintln(out, "\n[4/4] License Active.")
	fmt.Fprintln(out, "\nDONE. License is active. If deploy cne was interrupted, re-run it to complete the remaining steps (TMM restart, CNEInstance Available wait).")
	return nil
}
