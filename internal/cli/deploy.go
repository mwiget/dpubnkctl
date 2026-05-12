package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/mwiget/dpubnkctl/internal/deploy"
	"github.com/mwiget/dpubnkctl/internal/poc"
	"github.com/mwiget/dpubnkctl/internal/version"
)

func newDeployCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "deploy",
		Short: "Install the BNK platform (cert-manager, FLO, CNEInstance, VLANs, GatewayClass)",
	}
	cmd.AddCommand(newDeployPrereqsCmd())
	cmd.AddCommand(newDeployFLOCmd())
	cmd.AddCommand(newDeployCNECmd())
	return cmd
}

type deployPrereqsFlags struct {
	pocDir        string
	yolo          bool
	confirmDeploy string
	skipPull      bool
}

func newDeployPrereqsCmd() *cobra.Command {
	f := &deployPrereqsFlags{}
	cmd := &cobra.Command{
		Use:   "prereqs",
		Short: "BNK platform prerequisites: namespaces, FAR pull secret, cert-manager (DESTRUCTIVE)",
		Long: `Phase 4a — install everything BNK needs before the FLO chart:

  1. Inspect the JWT (prod vs tst) — operator confirms intent
  2. Extract the FAR pull secret from the tgz at poc.yaml.bnk.far_key_ref
  3. Create namespaces: f5-operators, f5-utils, bnk-gw, default
  4. Apply the FAR Secret (kubernetes.io/dockerconfigjson) to each
  5. helm install cert-manager (jetstack chart, with --set installCRDs=true)
  6. Wait for cert-manager-webhook to be Ready

Required gates:
  --yolo                  acknowledge that this writes to the cluster
  --confirm-deploy NAME   must equal poc.yaml.metadata.name`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDeployPrereqs(cmd.Context(), cmd.OutOrStdout(), f)
		},
	}
	cmd.Flags().StringVar(&f.pocDir, "poc", "", "PoC repo path (default: current directory)")
	cmd.Flags().BoolVar(&f.yolo, "yolo", false, "Acknowledge cluster writes")
	cmd.Flags().StringVar(&f.confirmDeploy, "confirm-deploy", "", "Must equal poc.yaml.metadata.name (typo guard)")
	cmd.Flags().BoolVar(&f.skipPull, "skip-pull", false, "Skip docker pull of kubectl + helm images")
	return cmd
}

func runDeployPrereqs(ctx context.Context, out io.Writer, f *deployPrereqsFlags) error {
	repo, err := resolvePoCDir(f.pocDir)
	if err != nil {
		return err
	}
	p, err := poc.Load(repo)
	if err != nil {
		return fmt.Errorf("not a PoC repo (%s): %w", repo, err)
	}

	if !f.yolo {
		return errors.New("refusing to write to cluster without --yolo")
	}
	if f.confirmDeploy != p.Metadata.Name {
		return fmt.Errorf("--confirm-deploy must equal poc.yaml.metadata.name (%q), got %q", p.Metadata.Name, f.confirmDeploy)
	}

	kubeconfig := filepath.Join(repo, "artifacts", "kubeconfig")
	if _, err := os.Stat(kubeconfig); err != nil {
		return fmt.Errorf("kubeconfig %s missing — run `dpubnkctl cluster up` first", kubeconfig)
	}

	farPath := resolveRef(repo, p.BNK.FARKeyRef)
	jwtPath := resolveRef(repo, p.BNK.JWTRef)
	for _, x := range []struct{ what, path string }{{"FAR tgz", farPath}, {"JWT", jwtPath}} {
		if _, err := os.Stat(x.path); err != nil {
			return fmt.Errorf("%s not found at %s — drop the file there and retry", x.what, x.path)
		}
	}

	fmt.Fprintf(out, "PoC:        %s\n", p.Metadata.Name)
	fmt.Fprintf(out, "Cluster:    %s\n", kubeconfig)
	fmt.Fprintf(out, "FAR tgz:    %s\n", farPath)
	fmt.Fprintf(out, "JWT:        %s\n\n", jwtPath)

	// 1. JWT inspection.
	fmt.Fprintln(out, "[1/5] Inspecting JWT ...")
	info, err := deploy.InspectJWT(jwtPath)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "      type=%s  iss=%v\n", info.Type, info.Claims["iss"])

	// 2. FAR extraction.
	fmt.Fprintln(out, "[2/5] Extracting FAR dockerconfigjson ...")
	dockerCfg, err := deploy.ExtractFARDockerConfig(farPath)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "      ok — %d bytes\n", len(dockerCfg))

	// 3. Tools preflight.
	r := &deploy.Runner{KubeconfigPath: kubeconfig, Out: prefixWriter{w: out, prefix: "      | "}}
	fmt.Fprintln(out, "[3/5] Tools preflight ...")
	if !f.skipPull {
		if err := r.CheckTools(ctx); err != nil {
			return err
		}
	}
	fmt.Fprintln(out, "      ok")

	// 4. Namespaces + FAR secret in each.
	fmt.Fprintln(out, "[4/5] Applying namespaces + FAR secret ...")
	namespaces := []string{"f5-operators", "f5-utils", "bnk-gw", "default"}
	for _, ns := range namespaces {
		if ns != "default" {
			if err := r.Apply(ctx, deploy.RenderNamespace(ns)); err != nil {
				return err
			}
		}
		if err := r.Apply(ctx, deploy.RenderFARSecret(ns, dockerCfg)); err != nil {
			return err
		}
		fmt.Fprintf(out, "      far-secret/%s ok\n", ns)
	}

	// 5. cert-manager.
	fmt.Fprintln(out, "[5/5] Installing cert-manager ...")
	if err := r.HelmUpgrade(ctx,
		"cert-manager",
		"cert-manager", // chart name (resolved via --repo)
		version.CertManagerRepo,
		"cert-manager",
		version.CertManagerVersion,
		"installCRDs: true\n",
	); err != nil {
		return err
	}
	if err := r.Wait(ctx, "cert-manager", "Available", "deployment/cert-manager-webhook", 5*time.Minute); err != nil {
		return err
	}
	fmt.Fprintln(out, "      cert-manager-webhook ready.")

	// poc.yaml status: prereqs done; full deploy is still in_progress.
	p.Status.Deploy = "in_progress"
	p.Status.LastPhaseAt = time.Now().UTC()
	if err := p.Save(repo); err != nil {
		return err
	}
	appendDeployJournal(repo, p.Metadata.Name, info.Type, "PREREQS COMPLETE", "")
	fmt.Fprintln(out, "\nDONE.  Next: `dpubnkctl deploy flo` (next iteration).")
	return nil
}

func resolveRef(repo, ref string) string {
	if filepath.IsAbs(ref) {
		return ref
	}
	return filepath.Join(repo, ref)
}

func appendDeployJournal(repo, pocName, jwtType, status, errMsg string) {
	date := time.Now().UTC().Format("2006-01-02")
	path := filepath.Join(repo, "journal", date+"-deploy.md")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "## lab-tech: deploy — %s\n", status)
	fmt.Fprintf(f, "- Time: %s\n", time.Now().UTC().Format(time.RFC3339))
	fmt.Fprintf(f, "- PoC:  %s\n", pocName)
	fmt.Fprintf(f, "- JWT type: %s\n", jwtType)
	if errMsg != "" {
		fmt.Fprintf(f, "- Error: %s\n", errMsg)
	}
	fmt.Fprintln(f)
}
