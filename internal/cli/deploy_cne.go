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
	pocDir          string
	yolo            bool
	confirmDeploy   string
	skipPull        bool
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
  3. Render + apply the GatewayClass (upstream Gateway-API v1) with the
     F5 CNE controllerName so FLO picks up Gateway objects that
     reference it.
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

	fmt.Fprintf(out, "PoC:        %s\n", p.Metadata.Name)
	fmt.Fprintf(out, "Cluster:    %s\n\n", kubeconfig)

	r := &deploy.Runner{KubeconfigPath: kubeconfig, Out: prefixWriter{w: out, prefix: "      | "}}

	if !f.skipPull {
		fmt.Fprintln(out, "[1/5] Tools preflight ...")
		if err := r.CheckTools(ctx); err != nil {
			return err
		}
	}
	fmt.Fprintln(out, "      ok")

	// 2. Apply CNEInstance — FLO watches it and reconciles the downstream
	//    BNK CRs (TMM, dssm, observer, ...). Lands in `default` per the
	//    cne-instance.yaml.tmpl namespace (commit 0270d78).
	fmt.Fprintln(out, "[2/5] Rendering + applying CNEInstance ...")
	cne, err := deploy.RenderCNEInstance(p)
	if err != nil {
		return err
	}
	if err := saveAndApply(ctx, r, repo, "artifacts/cne-instance-rendered.yaml", cne); err != nil {
		return err
	}

	// 3. Apply F5SPKVlan resources. TMM's bfd_watcher needs these to come
	//    out of "ERROR: vlan name not found" and let the readiness gates
	//    (RoutingDone / ConfigurationDone) flip to True. Aggregator keys
	//    by Role+Tag so a single VLAN spanning both DPUs gets one
	//    F5SPKVlan with both selfip_v4s. Skip if the PoC has no DPU
	//    VLANs declared (single-host or NIC-mode topologies).
	//
	//    The F5SPKVlan CRD is installed by FLO's reconciliation of the
	//    CNEInstance we applied in step 2. That reconciliation isn't
	//    instant — without a wait, `kubectl apply` here loses a race
	//    with "no matches for kind F5SPKVlan in version k8s.f5net.com/v1
	//    ensure CRDs are installed first" (caught dogfooding e2e on
	//    lake1). Wait until the CRD is Established.
	fmt.Fprintln(out, "[3/5] Rendering + applying F5SPKVlan(s) ...")
	if vlanCount := dpuVLANCount(p); vlanCount > 0 {
		fmt.Fprintln(out, "      Waiting for FLO to install F5SPKVlan CRD ...")
		if err := r.Wait(ctx, "", "Established",
			"crd/f5-spk-vlans.k8s.f5net.com", 5*time.Minute); err != nil {
			return fmt.Errorf("F5SPKVlan CRD did not become Established: %w", err)
		}
		vlans, err := deploy.RenderF5SPKVlans(p)
		if err != nil {
			return err
		}
		if err := saveAndApply(ctx, r, repo, "artifacts/f5spkvlans-rendered.yaml", vlans); err != nil {
			return err
		}
		fmt.Fprintf(out, "      applied %d aggregated F5SPKVlan(s).\n", vlanCount)
	} else {
		fmt.Fprintln(out, "      (no DPU VLANs in poc.yaml — skipped)")
	}

	// 4. Apply the upstream Gateway-API GatewayClass with the F5 CNE
	//    controllerName. FLO's f5-cne-controller registers itself under
	//    this name; once the GatewayClass exists, downstream Gateway
	//    objects can reference it via gatewayClassName. The historical
	//    BNKGatewayClassConfig CRD this used to also apply does not
	//    exist in BNK 2.2.0 — see AGENTS.md #20.
	fmt.Fprintln(out, "[4/5] Rendering + applying BNK GatewayClass ...")
	gwc, err := deploy.RenderGatewayClass("")
	if err != nil {
		return err
	}
	if err := saveAndApply(ctx, r, repo, "artifacts/bnk-gatewayclass-rendered.yaml", gwc); err != nil {
		return err
	}
	fmt.Fprintln(out, "      applied.")

	// 5. Wait for CNEInstance Ready. The CNEInstance lives in `default`
	//    (cne-instance.yaml.tmpl namespace), not f5-operators.
	fmt.Fprintln(out, "[5/5] Waiting for CNEInstance Ready ...")
	fmt.Fprintln(out, "      Requires Multus CNI + NADs in default (sf-external, sf-internal — installed by `deploy network`).")
	fmt.Fprintln(out, "      If TMM gates stay False, check license: kubectl -n f5-operators get secret activationstatus -o jsonpath='{.data.activationstatus}' | base64 -d")
	if err := r.Wait(ctx, "default", "Ready",
		"cneinstance/bnk-instance", f.cneReadyTimeout); err != nil {
		fmt.Fprintf(out, "      WARN: CNEInstance not Ready within %s — check `kubectl get cneinstance -A` + `kubectl -n default get pods` (%v)\n", f.cneReadyTimeout, err)
	} else {
		fmt.Fprintln(out, "      CNEInstance Ready.")
	}

	p.Status.Deploy = "completed"
	p.Status.LastPhaseAt = time.Now().UTC()
	if err := p.Save(repo); err != nil {
		return err
	}
	appendDeployJournal(repo, p.Metadata.Name, "", "BNK DEPLOYED", "")
	fmt.Fprintln(out, "\nDONE. BNK platform deployed. `kubectl get pods -A` to inspect TMM, CNE controller, and FLO.")
	return nil
}

// dpuVLANCount counts unique VLAN port names (Role+Tag) across all DPUs.
func dpuVLANCount(p *poc.PoC) int {
	seen := map[string]bool{}
	for _, h := range p.Hosts {
		for _, d := range h.DPUs {
			for _, v := range d.VLANs {
				seen[v.PortName()] = true
			}
		}
	}
	return len(seen)
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
