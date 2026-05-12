package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mwiget/dpubnkctl/internal/deploy"
	"github.com/mwiget/dpubnkctl/internal/poc"
	"github.com/mwiget/dpubnkctl/internal/version"
)

type deployFLOFlags struct {
	pocDir        string
	yolo          bool
	confirmDeploy string
	skipPull      bool
}

func newDeployFLOCmd() *cobra.Command {
	f := &deployFLOFlags{}
	cmd := &cobra.Command{
		Use:   "flo",
		Short: "Install bnk-ca cert-issuer chain + F5 Lifecycle Operator (DESTRUCTIVE)",
		Long: `Phase 4b.1 — install the F5 Lifecycle Operator on the cluster:

  1. Apply the bnk-ca cert-issuer chain to cert-manager:
       ClusterIssuer/selfsigned-bnk           (selfsigned root)
       Certificate/bnk-ca                     (CA cert + key)
       ClusterIssuer/bnk-ca-cluster-issuer    (CA referenced by FLO)
  2. Read JWT from poc.yaml.bnk.jwt_ref, classify prod vs tst.
  3. Render FLO values from the embedded template (matches
     f5-bnk-nvidia-bf3-installations v2.2.0-static).
  4. helm upgrade --install flo oci://repo.f5.com/charts/f5-lifecycle-operator
     into namespace f5-operators with the rendered values.
  5. Wait for the flo-controller deployment to become Available.

Required gates:
  --yolo                   acknowledge cluster writes
  --confirm-deploy NAME    must equal poc.yaml.metadata.name`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDeployFLO(cmd.Context(), cmd.OutOrStdout(), f)
		},
	}
	cmd.Flags().StringVar(&f.pocDir, "poc", "", "PoC repo path (default: current directory)")
	cmd.Flags().BoolVar(&f.yolo, "yolo", false, "Acknowledge cluster writes")
	cmd.Flags().StringVar(&f.confirmDeploy, "confirm-deploy", "", "Must equal poc.yaml.metadata.name (typo guard)")
	cmd.Flags().BoolVar(&f.skipPull, "skip-pull", false, "Skip docker pull of alpine/k8s image")
	return cmd
}

func runDeployFLO(ctx context.Context, out io.Writer, f *deployFLOFlags) error {
	repo, err := resolvePoCDir(f.pocDir)
	if err != nil {
		return err
	}
	p, err := poc.Load(repo)
	if err != nil {
		return fmt.Errorf("not a PoC repo (%s): %w", repo, err)
	}
	if !f.yolo {
		return errors.New("refusing destructive deploy without --yolo")
	}
	if f.confirmDeploy != p.Metadata.Name {
		return fmt.Errorf("--confirm-deploy must equal poc.yaml.metadata.name (%q), got %q", p.Metadata.Name, f.confirmDeploy)
	}

	kubeconfig := filepath.Join(repo, "artifacts", "kubeconfig")
	if _, err := os.Stat(kubeconfig); err != nil {
		return fmt.Errorf("kubeconfig %s missing — run `dpubnkctl cluster up` first", kubeconfig)
	}
	jwtPath := resolveRef(repo, p.BNK.JWTRef)
	if _, err := os.Stat(jwtPath); err != nil {
		return fmt.Errorf("JWT not found at %s — drop the file there and retry", jwtPath)
	}
	farPath := resolveRef(repo, p.BNK.FARKeyRef)
	if _, err := os.Stat(farPath); err != nil {
		return fmt.Errorf("FAR not found at %s — drop the file there and retry", farPath)
	}

	fmt.Fprintf(out, "PoC:        %s\n", p.Metadata.Name)
	fmt.Fprintf(out, "Cluster:    %s\n", kubeconfig)
	fmt.Fprintf(out, "JWT:        %s\n", jwtPath)
	fmt.Fprintf(out, "FLO chart:  %s @ %s\n\n", version.FLOChartName, p.Versions.FLOChart)

	r := &deploy.Runner{KubeconfigPath: kubeconfig, Out: prefixWriter{w: out, prefix: "      | "}}
	if !f.skipPull {
		fmt.Fprintln(out, "[1/5] Tools preflight ...")
		if err := r.CheckTools(ctx); err != nil {
			return err
		}
	}
	fmt.Fprintln(out, "      ok")

	// 2. Inspect JWT.
	fmt.Fprintln(out, "[2/5] Inspecting JWT (prod vs tst) ...")
	info, err := deploy.InspectJWT(jwtPath)
	if err != nil {
		return err
	}
	jwt, err := readJWT(jwtPath)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "      type=%s  iss=%v\n", info.Type, info.Claims["iss"])

	// 3. Apply cert-issuer chain.
	fmt.Fprintln(out, "[3/5] Applying bnk-ca cert-issuer chain ...")
	if err := r.Apply(ctx, deploy.CertIssuerChain()); err != nil {
		return err
	}
	// Wait for the CA Certificate to be Ready before FLO references it.
	if err := r.Wait(ctx, "cert-manager", "Ready", "certificate/bnk-ca", 3*time.Minute); err != nil {
		return fmt.Errorf("bnk-ca certificate did not become ready: %w", err)
	}
	fmt.Fprintln(out, "      bnk-ca-cluster-issuer ready.")

	// 4. Render values + helm install.
	fmt.Fprintln(out, "[4/5] Rendering FLO values + helm install ...")
	values, err := deploy.RenderFLOValues(info.Type, jwt)
	if err != nil {
		return err
	}
	// Save the rendered values for SE review (without raw JWT — but the
	// JWT is in the file. Mode 0600.)
	rendered := filepath.Join(repo, "artifacts", "flo-values-rendered.yaml")
	if err := os.MkdirAll(filepath.Dir(rendered), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(rendered, []byte(values), 0o600); err != nil {
		return err
	}
	fmt.Fprintf(out, "      rendered values → %s (mode 0600 — contains JWT)\n", rendered)
	// Helm needs to log in to repo.f5.com (OCI registry) before pulling.
	// The FAR SA JSON is the same credential as the image pull secret —
	// extract it and reuse for `helm registry login`.
	saJSON, err := extractFARServiceAccount(farPath)
	if err != nil {
		return fmt.Errorf("extract FAR service-account JSON: %w", err)
	}
	if err := r.HelmUpgradeOCI(ctx,
		deploy.OCIAuth{
			RegistryHost: "repo.f5.com",
			// _json_key (raw JSON password) — both auth forms work for
			// helm registry login; this one is simpler.
			Username: "_json_key",
			Password: saJSON,
		},
		"flo",
		version.FLOChartName,
		"f5-operators",
		p.Versions.FLOChart,
		values,
	); err != nil {
		return err
	}

	// 5. Wait for FLO controller. The chart names the deployment with
	// the release prefix, so it's "flo-f5-lifecycle-operator" not
	// "f5-lifecycle-operator". Use the label selector instead — the
	// chart sets app.kubernetes.io/name = f5-lifecycle-operator, so we
	// can wait by selector regardless of release name.
	fmt.Fprintln(out, "[5/5] Waiting for FLO controller to become Available ...")
	if err := r.Wait(ctx, "f5-operators", "Available",
		"deployment", 5*time.Minute,
		"-l", "app.kubernetes.io/name=f5-lifecycle-operator"); err != nil {
		fmt.Fprintf(out, "      WARN: FLO not Ready within 5 min — check `kubectl -n f5-operators get pods`. (%v)\n", err)
	} else {
		fmt.Fprintln(out, "      FLO controller Ready.")
	}

	p.Status.Deploy = "in_progress"
	p.Status.LastPhaseAt = time.Now().UTC()
	if err := p.Save(repo); err != nil {
		return err
	}
	appendDeployJournal(repo, p.Metadata.Name, info.Type, "FLO INSTALLED", "")
	fmt.Fprintln(out, "\nDONE.  Next: `dpubnkctl deploy cne` (next iteration).")
	return nil
}

// readJWT returns the JWT as a single-line string (trimmed).
func readJWT(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// extractFARServiceAccount returns the raw GCP service-account JSON from
// the FAR tgz (the same blob ExtractFARDockerConfig wraps in a
// dockerconfigjson). For helm registry login we need the raw JSON to use
// as `--password` against `_json_key` username.
func extractFARServiceAccount(tgzPath string) (string, error) {
	docker, err := deploy.ExtractFARDockerConfig(tgzPath)
	if err != nil {
		return "", err
	}
	// dockerConfig has auths.repo.f5.com.auth = base64("_json_key:<json>").
	// Decode and strip the prefix to get the raw SA JSON back.
	return deploy.UnwrapGARAuth(docker)
}
