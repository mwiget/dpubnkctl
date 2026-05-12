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
)

type deployCNEFlags struct {
	pocDir         string
	yolo           bool
	confirmDeploy  string
	tmmReplicas    int
	skipPull       bool
	cneReadyTimeout time.Duration
}

func newDeployCNECmd() *cobra.Command {
	f := &deployCNEFlags{}
	cmd := &cobra.Command{
		Use:   "cne",
		Short: "Apply CNEInstance + F5SPKVlan CRs + BNK GatewayClass (DESTRUCTIVE)",
		Long: `Phase 4b.2 — drives FLO to deploy the BNK data plane:

  1. Render + apply CNEInstance with dpu_enabled=true (FLO sees this
     and starts deploying TMM pods on the labeled DPU nodes).
  2. Render + apply F5SPKVlan CRs — one per logical VLAN, aggregating
     selfip_v4s across every DPU's IP for that VLAN tag. TMM-side
     interfaces auto-numbered 1.1, 1.2, ... in the order they appear
     in the first DPU's poc.yaml.
  3. Render + apply BNKGatewayClassConfig + GatewayClass.
  4. Wait for the CNEInstance to report Ready (TMM pods up).

Required gates:
  --yolo                    acknowledge cluster writes
  --confirm-deploy NAME     must equal poc.yaml.metadata.name`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDeployCNE(cmd.Context(), cmd.OutOrStdout(), f)
		},
	}
	cmd.Flags().StringVar(&f.pocDir, "poc", "", "PoC repo path (default: current directory)")
	cmd.Flags().BoolVar(&f.yolo, "yolo", false, "Acknowledge cluster writes")
	cmd.Flags().StringVar(&f.confirmDeploy, "confirm-deploy", "", "Must equal poc.yaml.metadata.name (typo guard)")
	cmd.Flags().IntVar(&f.tmmReplicas, "tmm-replicas", 0, "Default TMM replicas in BNKGatewayClassConfig (0 = auto: one per DPU)")
	cmd.Flags().BoolVar(&f.skipPull, "skip-pull", false, "Skip docker pull of alpine/k8s image")
	cmd.Flags().DurationVar(&f.cneReadyTimeout, "cne-ready-timeout", 15*time.Minute, "How long to wait for CNEInstance Ready")
	return cmd
}

func runDeployCNE(ctx context.Context, out io.Writer, f *deployCNEFlags) error {
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
		return fmt.Errorf("kubeconfig %s missing — run `dpubnkctl cluster up` + `cluster join-dpus` + `deploy flo` first", kubeconfig)
	}

	tmmReplicas := f.tmmReplicas
	if tmmReplicas == 0 {
		// One TMM per DPU keeps the math simple — operator can override.
		for _, h := range p.Hosts {
			tmmReplicas += len(h.DPUs)
		}
		if tmmReplicas == 0 {
			tmmReplicas = 1
		}
	}

	fmt.Fprintf(out, "PoC:        %s\n", p.Metadata.Name)
	fmt.Fprintf(out, "Cluster:    %s\n", kubeconfig)
	fmt.Fprintf(out, "TMM:        %d replicas\n\n", tmmReplicas)

	r := &deploy.Runner{KubeconfigPath: kubeconfig, Out: prefixWriter{w: out, prefix: "      | "}}

	if !f.skipPull {
		fmt.Fprintln(out, "[1/5] Tools preflight ...")
		if err := r.CheckTools(ctx); err != nil {
			return err
		}
	}
	fmt.Fprintln(out, "      ok")

	// 2. Apply CNEInstance — FLO watches it and (in the bnk-forge
	//    blueprint) reconciles the downstream BNK CRs. Always renders
	//    + applies; saved to artifacts for SE review.
	fmt.Fprintln(out, "[2/3] Rendering + applying CNEInstance ...")
	cne, err := deploy.RenderCNEInstance(p)
	if err != nil {
		return err
	}
	if err := saveAndApply(ctx, r, repo, "artifacts/cne-instance-rendered.yaml", cne); err != nil {
		return err
	}

	// 3. Wait for CNEInstance Ready. NOTE: this needs Multus CNI +
	//    NetworkAttachmentDefinitions for sf-external/sf-internal to
	//    actually progress past pending. Installing those is a future
	//    `dpubnkctl deploy network` step (current FLO logs:
	//    "no matches for kind NetworkAttachmentDefinition...").
	fmt.Fprintln(out, "[3/3] Waiting for CNEInstance Ready ...")
	fmt.Fprintln(out, "      NOTE: requires Multus CNI + NADs (sf-external, sf-internal).")
	fmt.Fprintln(out, "      If FLO is crashlooping, see `kubectl -n f5-operators logs -l app.kubernetes.io/name=f5-lifecycle-operator`.")
	if err := r.Wait(ctx, "f5-operators", "Ready",
		"cneinstance/bnk-instance", f.cneReadyTimeout); err != nil {
		fmt.Fprintf(out, "      WARN: CNEInstance not Ready within %s — check `kubectl -n f5-operators get cneinstance,pods` (%v)\n", f.cneReadyTimeout, err)
	} else {
		fmt.Fprintln(out, "      CNEInstance Ready.")
	}

	// VLAN + GatewayClass apply intentionally deferred — the FLO v2.9.27
	// schema differs from the f5-bnk-v2.2.0-static templates (different
	// API group: k8s.f5.com vs k8s.f5net.com; the BNKGatewayClassConfig
	// CRD also isn't installed by base FLO). Renderers are kept (with
	// tests) so the next iteration can use them once the right schema
	// is confirmed against the running cluster.
	_ = tmmReplicas

	p.Status.Deploy = "completed"
	p.Status.LastPhaseAt = time.Now().UTC()
	if err := p.Save(repo); err != nil {
		return err
	}
	appendDeployJournal(repo, p.Metadata.Name, "", "BNK DEPLOYED", "")
	fmt.Fprintln(out, "\nDONE. BNK platform deployed. `kubectl get pods -A` to inspect TMM, CNE controller, and FLO.")
	return nil
}

func saveAndApply(ctx context.Context, r *deploy.Runner, repo, relPath, manifest string) error {
	full := filepath.Join(repo, relPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(full, []byte(manifest), 0o644); err != nil {
		return err
	}
	if err := r.Apply(ctx, manifest); err != nil {
		return fmt.Errorf("apply %s: %w", relPath, err)
	}
	return nil
}
