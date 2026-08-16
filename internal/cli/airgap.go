package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/mwiget/dpubnkctl/internal/airgap"
	"github.com/mwiget/dpubnkctl/internal/deploy"
	"github.com/mwiget/dpubnkctl/internal/poc"
	"github.com/mwiget/dpubnkctl/internal/provision"
)

type airgapSetupFlags struct {
	pocDir string
	airgap string
}

func newAirgapCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "airgap",
		Short: "Airgap infrastructure management",
	}
	cmd.AddCommand(newAirgapSetupCmd(), newAirgapTeardownCmd(), newAirgapCleanCmd())
	return cmd
}

func newAirgapSetupCmd() *cobra.Command {
	f := &airgapSetupFlags{}
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Prepare local registry + file server for airgap deployment",
		Long: `Phase 0 — set up the local container registry and file server on the
jumphost so that all subsequent phases can pull images and binaries
without internet access on the target servers.

Two modes:
  --airgap online   Jumphost has internet. Download everything from
                    upstream registries, push to local registry, and
                    stage binaries on the local file server.
  --airgap offline  Jumphost has NO internet. Load from a pre-staged
                    directory (populated by a prior online run or the
                    manual download guide).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAirgapSetup(cmd.Context(), cmd.OutOrStdout(), f)
		},
	}
	cmd.Flags().StringVar(&f.pocDir, "poc", "", "PoC repo path (default: current directory)")
	cmd.Flags().StringVar(&f.airgap, "airgap", "", `Airgap mode: "online" or "offline"`)
	return cmd
}

func runAirgapSetup(ctx context.Context, w io.Writer, f *airgapSetupFlags) error {
	if f.airgap == "" {
		return fmt.Errorf("--airgap is required (online or offline)")
	}
	if f.airgap != airgap.ModeOnline && f.airgap != airgap.ModeOffline {
		return fmt.Errorf("--airgap must be %q or %q, got %q", airgap.ModeOnline, airgap.ModeOffline, f.airgap)
	}

	repo, err := resolvePoCDir(f.pocDir)
	if err != nil {
		return err
	}
	p, err := poc.Load(repo)
	if err != nil {
		return err
	}

	jumphostIP, autoDetected, err := resolveJumphostIP(p)
	if err != nil {
		return fmt.Errorf("cannot determine jumphost IP: %w", err)
	}
	if autoDetected {
		fmt.Fprintf(w, "auto-detected jumphost IP: %s (set airgap.jumphost_ip in poc.yaml to override)\n", jumphostIP)
	}

	p.Airgap = &poc.AirgapConfig{
		Mode:       f.airgap,
		JumphostIP: jumphostIP,
	}

	// Auto-set NFS fields for airgap — NFS server is the host (reachable
	// from DPU via tmfifo), export path defaults to /srv/nfs/f5-bnk.
	if p.Network.NFSServer == "" {
		p.Network.NFSServer = "192.168.100.1"
	}
	if p.Network.NFSPath == "" {
		p.Network.NFSPath = "/srv/nfs/f5-bnk"
	}

	cfg := airgap.NewConfig(repo, p)
	staging := cfg.StagingDir

	fmt.Fprintf(w, "=== Phase 0: airgap setup (mode=%s, jumphost=%s) ===\n\n", f.airgap, jumphostIP)

	// Step 0: Prerequisites
	farPath := filepath.Join(repo, p.BNK.FARKeyRef)
	if err := airgap.CheckPrerequisites(ctx, w, f.airgap, farPath); err != nil {
		return err
	}
	fmt.Fprintln(w, "")

	// Step 1: TLS certs
	fmt.Fprintln(w, "[1/11] Generating TLS certificates ...")
	if err := airgap.GenerateCerts(ctx, w, cfg.CertDir, jumphostIP); err != nil {
		return err
	}

	// Step 2: Start registry
	fmt.Fprintln(w, "\n[2/11] Starting local container registry ...")
	if err := airgap.StartRegistry(ctx, w, cfg.CertDir); err != nil {
		return err
	}

	if f.airgap == airgap.ModeOnline {
		// Step 3: Download + push kubespray images + cert-manager images
		fmt.Fprintln(w, "\n[3/11] Downloading and pushing kubespray + cert-manager images ...")
		if err := airgap.DownloadAndPushImages(ctx, w, cfg); err != nil {
			return err
		}

		// Step 4: Download binaries
		fmt.Fprintln(w, "\n[4/11] Downloading kubespray binaries ...")
		if err := airgap.DownloadBinaries(ctx, w, cfg); err != nil {
			return err
		}

		// Step 5: Download BFB image
		fmt.Fprintln(w, "\n[5/11] Downloading BFB image ...")
		bfbCacheDir := p.Provisioning.BFBCacheDir
		if bfbCacheDir == "" {
			bfbCacheDir = "~/.cache/dpubnkctl/bfb"
		}
		bfbPath, bfbErr := provision.EnsureBFB(ctx, bfbCacheDir, p.Versions.BFBImage, "", func(written, total int64) {
			if total > 0 {
				fmt.Fprintf(w, "  %d%%\r", written*100/total)
			}
		})
		if bfbErr != nil {
			return fmt.Errorf("download BFB: %w", bfbErr)
		}
		fmt.Fprintf(w, "  cached at %s\n", bfbPath)

		// Step 6: Download + push BNK host images
		fmt.Fprintln(w, "\n[6/11] Downloading BNK host images (amd64, FAR auth) ...")
		if err := loginFAR(ctx, w, farPath); err != nil {
			return err
		}
		if err := airgap.DownloadAndPushBNKHostImages(ctx, w, cfg); err != nil {
			return err
		}

		// Step 7: Download BNK deploy artifacts (manifest, charts, cert-manager)
		fmt.Fprintln(w, "\n[7/11] Downloading BNK deploy artifacts ...")
		farAuth, err := deploy.ExtractFARRegistryAuth(farPath)
		if err != nil {
			return fmt.Errorf("extract FAR auth: %w", err)
		}
		if err := airgap.DownloadReleaseManifest(ctx, w, cfg, farAuth); err != nil {
			return fmt.Errorf("release manifest: %w", err)
		}
		if err := airgap.DownloadFLOChart(ctx, w, cfg, farAuth); err != nil {
			return fmt.Errorf("FLO chart: %w", err)
		}
		if err := airgap.DownloadCertManagerManifest(ctx, w, cfg); err != nil {
			return fmt.Errorf("cert-manager manifest: %w", err)
		}
		if err := airgap.DownloadCertGenChart(ctx, w, cfg, farAuth); err != nil {
			return fmt.Errorf("cert-gen chart: %w", err)
		}
		if err := airgap.DownloadTigeraOperatorManifest(ctx, w, cfg); err != nil {
			return fmt.Errorf("tigera-operator manifest: %w", err)
		}
		if err := airgap.DownloadNFSCSIChart(ctx, w, cfg); err != nil {
			return fmt.Errorf("NFS CSI chart: %w", err)
		}

		// Step 8: Download BNK DPU images
		fmt.Fprintln(w, "\n[8/11] Downloading BNK DPU images (arm64) ...")
		if err := airgap.DownloadBNKDPUImages(ctx, w, cfg); err != nil {
			return err
		}

		// Step 9: Download DPU kubespray images (arm64)
		fmt.Fprintln(w, "\n[9/11] Downloading DPU kubespray images (arm64) ...")
		if err := airgap.DownloadDPUKubesprayImages(ctx, w, cfg); err != nil {
			return err
		}

		// Step 10: Download DPU debs (arm64)
		fmt.Fprintln(w, "\n[10/11] Downloading DPU debs (arm64) ...")
		if err := airgap.DownloadDPUDebs(ctx, w, cfg); err != nil {
			return err
		}
	} else {
		// Offline mode: validate pre-staged content
		fmt.Fprintln(w, "\n[3/11] Validating pre-staged packages ...")
		if err := airgap.ValidatePreStaged(w, staging); err != nil {
			return err
		}

		// Step 4: Load pre-staged images into registry
		fmt.Fprintln(w, "\n[4/11] Loading pre-staged images into registry ...")
		if err := airgap.LoadPreStagedImages(ctx, w, cfg); err != nil {
			return err
		}

		fmt.Fprintln(w, "\n[5/11] Skipped (offline mode)")
		fmt.Fprintln(w, "[6/11] Skipped (offline mode)")
		fmt.Fprintln(w, "[7/11] Skipped (offline mode)")
		fmt.Fprintln(w, "[8/11] Skipped (offline mode)")
		fmt.Fprintln(w, "[9/11] Skipped (offline mode)")
		fmt.Fprintln(w, "[10/11] Skipped (offline mode)")
	}

	// Step 11: Populate + start file server
	fmt.Fprintln(w, "\n[11/11] Starting file server ...")
	if err := airgap.PopulateAndStartFileServer(ctx, w, cfg); err != nil {
		return err
	}

	// Save airgap config to poc.yaml
	if err := p.Save(repo); err != nil {
		return fmt.Errorf("save poc.yaml: %w", err)
	}

	fmt.Fprintln(w, "\n=== Phase 0 complete ===")
	fmt.Fprintf(w, "  registry: https://%s\n", cfg.RegistryHost)
	fmt.Fprintf(w, "  fileserver: %s\n", cfg.FilesRepo)
	return nil
}

func resolveJumphostIP(p *poc.PoC) (ip string, autoDetected bool, err error) {
	if p.Airgap != nil && p.Airgap.JumphostIP != "" {
		return p.Airgap.JumphostIP, false, nil
	}
	if len(p.Hosts) > 0 && p.Hosts[0].SSH.Jumphost != "" {
		return p.Hosts[0].SSH.Jumphost, false, nil
	}
	// Auto-detect: find the local IP on the same subnet as the first host
	if len(p.Hosts) > 0 && p.Hosts[0].SSH.Address != "" {
		detected, dialErr := localIPForHost(p.Hosts[0].SSH.Address)
		if dialErr == nil && detected != "" {
			return detected, true, nil
		}
	}
	return "", false, fmt.Errorf("set airgap.jumphost_ip in poc.yaml or configure hosts[0].ssh.jumphost")
}

func localIPForHost(hostAddr string) (string, error) {
	conn, err := net.DialTimeout("udp", hostAddr+":22", 2*time.Second)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	localAddr := conn.LocalAddr().(*net.UDPAddr)
	return localAddr.IP.String(), nil
}

func loginFAR(ctx context.Context, w io.Writer, farTgzPath string) error {
	auth, err := deploy.ExtractFARRegistryAuth(farTgzPath)
	if err != nil {
		return fmt.Errorf("extract FAR auth: %w", err)
	}
	fmt.Fprintf(w, "  logging into %s ...\n", auth.RegistryHost)
	// skopeo login is handled by skopeo copy when credentials are in the auth store.
	// For FAR, we do a manual skopeo login.
	return skopeoLogin(ctx, w, auth.RegistryHost, auth.Username, auth.Password)
}

func skopeoLogin(ctx context.Context, w io.Writer, host, user, password string) error {
	cmd := execCommand(ctx, "skopeo", "login", host,
		"--username", user,
		"--password-stdin")
	cmd.Stdin = stringReader(password + "\n")
	cmd.Stdout = w
	cmd.Stderr = w
	return cmd.Run()
}

type airgapTeardownFlags struct {
	pocDir string
}

func newAirgapTeardownCmd() *cobra.Command {
	f := &airgapTeardownFlags{}
	cmd := &cobra.Command{
		Use:   "teardown",
		Short: "Stop and remove the local registry + file server containers",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAirgapTeardown(cmd.Context(), cmd.OutOrStdout(), f)
		},
	}
	cmd.Flags().StringVar(&f.pocDir, "poc", "", "PoC repo path (default: current directory)")
	return cmd
}

func runAirgapTeardown(ctx context.Context, w io.Writer, f *airgapTeardownFlags) error {
	fmt.Fprintln(w, "=== Airgap teardown ===")
	_ = airgap.StopRegistry(ctx, w)
	_ = airgap.StopFileServer(ctx, w)
	fmt.Fprintln(w, "airgap infrastructure removed")
	return nil
}

type airgapCleanFlags struct {
	pocDir string
}

func newAirgapCleanCmd() *cobra.Command {
	f := &airgapCleanFlags{}
	cmd := &cobra.Command{
		Use:   "clean",
		Short: "Stop containers and remove all airgap staging artifacts for a fresh start",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAirgapClean(cmd.Context(), cmd.OutOrStdout(), f)
		},
	}
	cmd.Flags().StringVar(&f.pocDir, "poc", "", "PoC repo path (default: current directory)")
	return cmd
}

func runAirgapClean(ctx context.Context, w io.Writer, f *airgapCleanFlags) error {
	repo, err := resolvePoCDir(f.pocDir)
	if err != nil {
		return err
	}

	fmt.Fprintln(w, "=== Airgap clean ===")

	_ = airgap.StopRegistry(ctx, w)
	_ = airgap.StopFileServer(ctx, w)

	staging := filepath.Join(repo, airgap.StagingDir)
	fmt.Fprintf(w, "removing %s ...\n", staging)
	if err := os.RemoveAll(staging); err != nil {
		return fmt.Errorf("remove staging dir: %w", err)
	}

	fmt.Fprintln(w, "airgap clean complete — ready for a fresh Phase 0")
	return nil
}
