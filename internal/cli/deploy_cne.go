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
)

type deployCNEFlags struct {
	pocDir              string
	yolo                bool
	confirmDeploy       string
	skipPull            bool
	cneReadyTimeout     time.Duration
	licenseReadyTimeout time.Duration
	licenseMode         string
}

func newDeployCNECmd() *cobra.Command {
	f := &deployCNEFlags{}
	cmd := &cobra.Command{
		Use:   "cne",
		Short: "Apply CNEInstance + F5SPKVlan CRs + BNK GatewayClass + License CR (DESTRUCTIVE)",
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
  4. Wait for the CNEInstance to report Available.
  5. Apply the License CR (k8s.f5net.com/v1) with the JWT from
     poc.yaml.bnk.jwt_ref. CWC validates against the JWT-derived TEEM
     endpoint and flips .status.state to Active. (Disconnected-mode
     customers stay at PendingVerification — run the manual licensing
     curl ritual from F5's docs to finish.)

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
	// License registration with F5's licensing server (Connected mode)
	// takes 5-15 minutes on a first-time deploy — CWC has to register a
	// new digital asset before the license can flip to Active. Default
	// 15 min covers the slow path; operators can shorten when they
	// know they're re-applying.
	cmd.Flags().DurationVar(&f.licenseReadyTimeout, "license-ready-timeout", 15*time.Minute, "How long to wait for the License CR to reach Active")
	cmd.Flags().StringVar(&f.licenseMode, "license-mode", "connected", "License CR operationMode: connected or disconnected")
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
	if err := enforceValidateForPhase(out, p, repo, poc.PhaseDeploy, false); err != nil {
		return err
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
		// FLO doesn't apply the F5SPKVlan CRD inline with its own helm
		// release — a `crd-installer` Job inside f5-operators races to
		// reconcile it after the CNEInstance lands. `kubectl wait
		// --for=condition=Established` errors immediately ("error: no
		// matching resources found") when the CRD object itself doesn't
		// yet exist, so we have to two-step: wait for the CRD to be
		// created at all, then wait for it to be Established.
		fmt.Fprintln(out, "      Waiting for FLO crd-installer to create the F5SPKVlan CRD ...")
		if err := r.Kubectl(ctx, "wait", "--for=create",
			"crd/f5-spk-vlans.k8s.f5net.com", "--timeout=5m"); err != nil {
			return fmt.Errorf("F5SPKVlan CRD never created (check `kubectl -n f5-operators get jobs` for FLO crd-installer status): %w", err)
		}
		fmt.Fprintln(out, "      Waiting for F5SPKVlan CRD Established ...")
		if err := r.Wait(ctx, "", "Established",
			"crd/f5-spk-vlans.k8s.f5net.com", 5*time.Minute); err != nil {
			return fmt.Errorf("F5SPKVlan CRD did not become Established: %w", err)
		}
		// FLO also reconciles a `f5-cne-controller` Deployment (in the
		// CNEInstance's namespace, `default`) that hosts the validating
		// admission webhook `f5validate.f5net.com` at
		// f5-validation-svc:3340/f5-validator. F5SPKVlan creates go
		// through that webhook — if the backing pod isn't Ready yet,
		// kubectl apply fails with "failed calling webhook ... dial tcp
		// ...: connect: connection refused". Wait for both the
		// deployment to exist AND become Available before applying.
		fmt.Fprintln(out, "      Waiting for f5-cne-controller deployment (hosts the admission webhook) ...")
		if err := r.Kubectl(ctx, "wait", "-n", "default", "--for=create",
			"deployment/f5-cne-controller", "--timeout=3m"); err != nil {
			return fmt.Errorf("f5-cne-controller deployment was not created: %w", err)
		}
		if err := r.Wait(ctx, "default", "Available",
			"deployment/f5-cne-controller", 5*time.Minute); err != nil {
			return fmt.Errorf("f5-cne-controller deployment did not become Available: %w", err)
		}
		vlans, err := deploy.RenderF5SPKVlans(p)
		if err != nil {
			return err
		}
		// Even after deployment/f5-cne-controller is Available, the
		// validating-webhook server inside the pod may take a few
		// seconds more to bind to port 3340 (the deployment's readiness
		// probe doesn't actually probe the webhook listener). First
		// applies tend to hit "failed calling webhook ... connection
		// refused". Retry with a short backoff specifically on that
		// error; surface any other failure immediately.
		var applyErr error
		for attempt := 1; attempt <= 30; attempt++ {
			applyErr = saveAndApply(ctx, r, repo, "artifacts/f5spkvlans-rendered.yaml", vlans)
			if applyErr == nil {
				break
			}
			msg := applyErr.Error()
			if !strings.Contains(msg, "f5validate.f5net.com") || !strings.Contains(msg, "connection refused") {
				return applyErr
			}
			fmt.Fprintf(out, "      webhook not bound yet, retrying in 5s (attempt %d/30) ...\n", attempt)
			time.Sleep(5 * time.Second)
		}
		if applyErr != nil {
			return fmt.Errorf("F5SPKVlan apply: webhook never came up: %w", applyErr)
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

	// 5. Wait for CNEInstance Available. The CNEInstance reports its
	//    overall readiness via the `Available` condition (NOT `Ready`
	//    — that condition name does not exist on CNEInstance). Available
	//    flips True when every component condition (F5Tmm, NodeLabeler,
	//    CRDInstaller, CNEController, Afm, Downloader, DSSM, Rabbitmq,
	//    CRDConversion, Cwc, IPAMController, Observer, OtelCollector,
	//    CSRC, …) reaches True. The CNEInstance lives in `default`
	//    (cne-instance.yaml.tmpl namespace), not f5-operators.
	fmt.Fprintln(out, "[5/5] Waiting for CNEInstance Available ...")
	fmt.Fprintln(out, "      Requires Multus CNI + NADs in default (sf-external, sf-internal — installed by `deploy network`).")
	fmt.Fprintln(out, "      If TMM gates stay False, check license: kubectl -n f5-operators get secret activationstatus -o jsonpath='{.data.activationstatus}' | base64 -d")
	if err := r.Wait(ctx, "default", "Available",
		"cneinstance/bnk-instance", f.cneReadyTimeout); err != nil {
		fmt.Fprintf(out, "      WARN: CNEInstance not Available within %s — check `kubectl get cneinstance -A` + `kubectl -n default get pods` (%v)\n", f.cneReadyTimeout, err)
	} else {
		fmt.Fprintln(out, "      CNEInstance Available.")
	}

	// 6. License CR. New in 2.3: the JWT no longer lives in FLO chart
	// values; it goes into a License custom resource (k8s.f5net.com/v1)
	// in the shared-component namespace. CWC watches the CR, validates
	// the JWT, contacts the TEEM endpoint derived from the JWT's jku
	// header (so prod vs tst is auto), and updates .status.state →
	// PendingVerification → Active.
	//
	// The License CRD is installed by FLO's crd-installer reconciliation
	// (same pattern as F5SPKVlan in step 3). Two-step wait again so the
	// kubectl apply doesn't race the CRD's appearance.
	jwtPath := resolveRef(repo, p.BNK.JWTRef)
	if _, err := os.Stat(jwtPath); err != nil {
		fmt.Fprintf(out, "[6/6] License CR — JWT not at %s, skipping (deploy flo will have warned)\n", jwtPath)
		fmt.Fprintln(out, "\nDONE. BNK platform deployed (no license applied). `kubectl get pods -A` to inspect.")
		return nil
	}
	jwt, err := readJWT(jwtPath)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "[6/6] Applying License CR (mode=%s) ...\n", f.licenseMode)
	fmt.Fprintln(out, "      Waiting for license CRD ...")
	if err := r.Kubectl(ctx, "wait", "--for=create",
		"crd/licenses.k8s.f5net.com", "--timeout=3m"); err != nil {
		return fmt.Errorf("license CRD never created (FLO crd-installer didn't reconcile it?): %w", err)
	}
	if err := r.Wait(ctx, "", "Established",
		"crd/licenses.k8s.f5net.com", 3*time.Minute); err != nil {
		return fmt.Errorf("license CRD did not become Established: %w", err)
	}
	licenseYAML, err := deploy.RenderLicenseCR(deploy.LicenseInputs{
		Namespace:     deploy.SharedComponentNamespace,
		OperationMode: f.licenseMode,
		JWT:           jwt,
	})
	if err != nil {
		return err
	}
	// Persist the rendered License CR for audit; mode 0600 since it
	// embeds the raw JWT.
	licenseRendered := filepath.Join(repo, "artifacts", "license-cr-rendered.yaml")
	if err := os.WriteFile(licenseRendered, []byte(licenseYAML), 0o600); err != nil {
		return err
	}
	if err := r.Apply(ctx, licenseYAML); err != nil {
		return fmt.Errorf("apply license CR: %w", err)
	}
	fmt.Fprintln(out, "      License CR applied; waiting for state=Active ...")
	if err := deploy.WaitForLicenseActive(ctx, r,
		deploy.LicenseCRName, deploy.SharedComponentNamespace,
		f.licenseReadyTimeout); err != nil {
		if errors.Is(err, deploy.ErrLicensePendingVerification) {
			fmt.Fprintln(out, "      WARN: license stuck at PendingVerification — disconnected-mode operator action required (see F5 docs §pg-install-bnk-dpu-kubernetes-flo-install-license-your-cluster-flo).")
		} else {
			fmt.Fprintf(out, "      WARN: license did not reach Active within %s — `kubectl -n %s describe license %s` (%v)\n",
				f.licenseReadyTimeout, deploy.SharedComponentNamespace, deploy.LicenseCRName, err)
		}
	} else {
		fmt.Fprintln(out, "      License Active.")
	}

	p.Status.Deploy = "completed"
	p.Status.LastPhaseAt = time.Now().UTC()
	if err := savePoC(repo, p, out); err != nil {
		return err
	}
	appendDeployJournal(repo, p.Metadata.Name, "", "BNK DEPLOYED", "")
	fmt.Fprintln(out, "\nDONE. BNK platform deployed. `kubectl get pods -A` to inspect TMM, CNE controller, FLO, and license.")
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
