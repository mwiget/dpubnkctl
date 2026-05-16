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
		Short: "Install F5 Lifecycle Operator + CWC API certs (DESTRUCTIVE)",
		Long: `Phase 4b.1 — install the F5 Lifecycle Operator on the cluster:

  1. Tools preflight (alpine/k8s container).
  2. Pull release-manifest (` + version.CNEManifestVersion + `) to resolve
     the FLO + cert-gen chart versions for this BNK release. Caches to
     <poc>/artifacts/release-manifest/, persists p.Versions.FLOChart back
     to poc.yaml.
  3. Read JWT from poc.yaml.bnk.jwt_ref (diagnostic only — the JWT now
     drives the License CR in step 10, not the FLO chart values).
  4. Ensure prereq namespaces: cert-manager, f5-operators, ` +
			deploy.SharedComponentNamespace + `.
  5. Install cert-manager (pinned upstream manifest).
  6. Apply the bnk-ca cert-issuer chain in cert-manager.
  7. Create far-secret (image-pull credential for repo.f5.com) in
     f5-operators, default, and ` + deploy.SharedComponentNamespace + `.
  8. Render FLO values from the embedded template (single shape — prod
     and tst converge here in 2.3) and helm install at the
     manifest-resolved version.
  9. Wait for the FLO controller to become Available.
 10. Pull f5-cert-gen chart, run gen_cert.sh -s=api-server, apply the
     cwc-license-certs + cwc-license-client-certs secrets to
     ` + deploy.SharedComponentNamespace + ` so CWC has its TLS material
     by the time CNEInstance reconciliation brings it up.

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
	if err := enforceValidateForPhase(out, p, repo, poc.PhaseDeploy, false); err != nil {
		return err
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
	fmt.Fprintf(out, "Manifest:   %s\n\n", version.CNEManifestVersion)

	r := &deploy.Runner{KubeconfigPath: kubeconfig, Out: prefixWriter{w: out, prefix: "      | "}}
	if !f.skipPull {
		fmt.Fprintln(out, "[1/10] Tools preflight ...")
		if err := r.CheckTools(ctx); err != nil {
			return err
		}
	}
	fmt.Fprintln(out, "      ok")

	// 2. Pull release-manifest. Resolves FLO + cert-gen chart versions
	// from F5's bill-of-materials chart; without this we can't pin the
	// helm install or know which f5-cert-gen revision to run.
	fmt.Fprintln(out, "[2/10] Pulling release-manifest from repo.f5.com ...")
	farAuth, err := deploy.ExtractFARRegistryAuth(farPath)
	if err != nil {
		return fmt.Errorf("extract FAR registry credentials: %w", err)
	}
	mfCache := filepath.Join(repo, "artifacts", "release-manifest")
	manifest, err := deploy.PullReleaseManifest(ctx, farAuth, version.CNEManifestVersion, mfCache)
	if err != nil {
		return fmt.Errorf("pull release-manifest: %w", err)
	}
	floVer := manifest.Chart("charts/f5-lifecycle-operator")
	certGenVer := manifest.Chart("utils/f5-cert-gen")
	if floVer == "" {
		return fmt.Errorf("release-manifest %s lists no charts/f5-lifecycle-operator entry — bug in F5 manifest?", manifest.Version)
	}
	if certGenVer == "" {
		return fmt.Errorf("release-manifest %s lists no utils/f5-cert-gen entry — bug in F5 manifest?", manifest.Version)
	}
	fmt.Fprintf(out, "      FLO chart      %s\n", floVer)
	fmt.Fprintf(out, "      f5-cert-gen    %s\n", certGenVer)
	// Persist resolved FLO chart into poc.yaml so cluster_status / e2e
	// report / future runs all see the same version.
	p.Versions.FLOChart = floVer
	if err := savePoC(repo, p, out); err != nil {
		return err
	}

	// 3. Inspect JWT (diagnostic only in 2.3 — the TEEM endpoint is
	// derived by CWC from the JWT's jku at runtime, see AGENTS.md #15).
	fmt.Fprintln(out, "[3/10] Inspecting JWT (diagnostic) ...")
	info, err := deploy.InspectJWT(jwtPath)
	if err != nil {
		return err
	}
	// Read the JWT so we can flag obvious corruption now (rather than
	// 20 minutes into deploy cne when the License CR is applied). The
	// raw bytes are used later by deploy_cne via resolveRef + readJWT;
	// here we only validate.
	if _, err := readJWT(jwtPath); err != nil {
		return err
	}
	fmt.Fprintf(out, "      type=%s  jku=%s  sub=%s\n", info.Type, info.JKU, info.Sub)

	// 4. Ensure prereq namespaces. cert-manager + f5-operators are the
	// historical ones; shared-component (f5-cne-core) is new in 2.3 for
	// CWC + License + observer.
	fmt.Fprintln(out, "[4/10] Ensuring prereq namespaces exist ...")
	for _, ns := range []string{"cert-manager", "f5-operators", deploy.SharedComponentNamespace} {
		if err := r.Apply(ctx, deploy.RenderNamespace(ns)); err != nil {
			return fmt.Errorf("create namespace %s: %w", ns, err)
		}
	}
	fmt.Fprintln(out, "      ok")

	// 5. cert-manager.
	fmt.Fprintf(out, "[5/10] Installing cert-manager %s ...\n", version.CertManagerVersion)
	cmURL := fmt.Sprintf("https://github.com/cert-manager/cert-manager/releases/download/%s/cert-manager.yaml", version.CertManagerVersion)
	if err := r.Kubectl(ctx, "apply", "-f", cmURL); err != nil {
		return fmt.Errorf("install cert-manager: %w", err)
	}
	if err := r.Wait(ctx, "cert-manager", "Available", "deployment", 5*time.Minute,
		"-l", "app.kubernetes.io/instance=cert-manager"); err != nil {
		return fmt.Errorf("cert-manager deployments did not become Available: %w", err)
	}
	fmt.Fprintln(out, "      cert-manager Ready.")

	// 6. Bnk-ca cert-issuer chain.
	fmt.Fprintln(out, "[6/10] Applying bnk-ca cert-issuer chain ...")
	if err := r.Apply(ctx, deploy.CertIssuerChain()); err != nil {
		return err
	}
	if err := r.Wait(ctx, "cert-manager", "Ready", "certificate/bnk-ca", 3*time.Minute); err != nil {
		return fmt.Errorf("bnk-ca certificate did not become ready: %w", err)
	}
	fmt.Fprintln(out, "      bnk-ca-cluster-issuer ready.")

	// 7. far-secret in every namespace that pulls F5 images. In 2.3 the
	// new shared-component namespace also pulls CWC + observer images.
	fmt.Fprintf(out, "[7/10] Creating far-secret in f5-operators + default + %s ...\n", deploy.SharedComponentNamespace)
	dockerCfg, err := deploy.ExtractFARDockerConfig(farPath)
	if err != nil {
		return fmt.Errorf("extract FAR dockerconfigjson: %w", err)
	}
	for _, ns := range []string{"f5-operators", "default", deploy.SharedComponentNamespace} {
		if err := r.Apply(ctx, deploy.RenderFARSecret(ns, dockerCfg)); err != nil {
			return fmt.Errorf("create far-secret in %s: %w", ns, err)
		}
	}
	fmt.Fprintln(out, "      far-secret in place.")

	// 8. Render FLO values + helm install. Single template in 2.3 (no
	// prod/tst split — the license/TEEM block moved to the License CR
	// which CWC reconciles after CNEInstance brings it up).
	fmt.Fprintln(out, "[8/10] Rendering FLO values + helm install ...")
	values, err := deploy.RenderFLOValues(deploy.FLOInputs{
		Namespace:                "f5-operators",
		SharedComponentNamespace: deploy.SharedComponentNamespace,
		ClusterIssuer:            "bnk-ca-cluster-issuer",
	})
	if err != nil {
		return err
	}
	rendered := filepath.Join(repo, "artifacts", "flo-values-rendered.yaml")
	if err := os.MkdirAll(filepath.Dir(rendered), 0o755); err != nil {
		return err
	}
	// No JWT in rendered values anymore — safe 0644.
	if err := os.WriteFile(rendered, []byte(values), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(out, "      rendered values → %s\n", rendered)
	if err := r.HelmUpgradeOCI(ctx, farAuth,
		"flo",
		version.FLOChartOCIRef,
		"f5-operators",
		floVer,
		values,
	); err != nil {
		return err
	}

	// 9. Wait for FLO controller. The chart names the deployment with
	// the release prefix, so it's "flo-f5-lifecycle-operator" not
	// "f5-lifecycle-operator". The chart sets
	// app.kubernetes.io/name = f5-lifecycle-operator, so a label
	// selector works regardless of release name.
	fmt.Fprintln(out, "[9/10] Waiting for FLO controller Available ...")
	if err := r.Wait(ctx, "f5-operators", "Available", "deployment", 5*time.Minute,
		"-l", "app.kubernetes.io/name=f5-lifecycle-operator"); err != nil {
		fmt.Fprintf(out, "      WARN: FLO not Ready within 5 min — check `kubectl -n f5-operators get pods`. (%v)\n", err)
	} else {
		fmt.Fprintln(out, "      FLO controller Ready.")
	}

	// 10. CWC cert preflight. f5-cert-gen helm-pull + gen_cert.sh +
	// kubectl apply the two secrets to the shared-component namespace.
	// CWC's pods (brought up by CNEInstance reconciliation in the next
	// phase) mount these secrets — without them, the CWC API listener
	// can't start and CNEInstance Available never flips True.
	fmt.Fprintln(out, "[10/10] Generating + applying CWC API certs ...")
	certsWorkDir := filepath.Join(repo, "artifacts", "cwc-certs")
	if err := deploy.PullAndApplyCWCCerts(ctx, r, farAuth, certGenVer, certsWorkDir,
		deploy.SharedComponentNamespace, prefixWriter{w: out, prefix: "      | "}); err != nil {
		return fmt.Errorf("CWC cert preflight: %w", err)
	}
	fmt.Fprintln(out, "      cwc-license-certs + cwc-license-client-certs applied.")

	p.Status.Deploy = "in_progress"
	p.Status.LastPhaseAt = time.Now().UTC()
	if err := savePoC(repo, p, out); err != nil {
		return err
	}
	appendDeployJournal(repo, p.Metadata.Name, info.Type, "FLO INSTALLED + CWC CERTS APPLIED", "")
	fmt.Fprintln(out, "\nDONE.  Next: `dpubnkctl deploy cne` (applies CNEInstance + License CR).")
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

// floChartLabel is the shared formatter used by cluster_status, e2e
// report rendering, and the FLO deploy banner.
func floChartLabel(p *poc.PoC) string {
	if v := strings.TrimSpace(p.Versions.FLOChart); v != "" {
		return v
	}
	return "(unresolved — pulled from release-manifest " + version.CNEManifestVersion + " at deploy time)"
}
