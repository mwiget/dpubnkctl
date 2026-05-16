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

  1. Apply the License CR (k8s.f5net.com/v1) with the JWT from
     poc.yaml.bnk.jwt_ref and wait for state=Active. CWC validates
     against the JWT-derived TEEM endpoint. Must happen BEFORE the
     CNEInstance — otherwise TMM pods come up unlicensed, their
     bfd_watcher checks for VLAN config before CWC starts pushing,
     gives up, and the RoutingDone readiness gate never flips True.
     (Disconnected-mode customers stay at PendingVerification — run
     the manual licensing curl ritual from F5's docs to finish.)
  2. Render + apply CNEInstance with dpu_enabled=true (FLO sees this
     and starts deploying TMM pods on the labeled DPU nodes).
  3. Render + apply F5SPKVlan CRs — one per logical VLAN, aggregating
     selfip_v4s across every DPU's IP for that VLAN tag. TMM-side
     interfaces auto-numbered 1.1, 1.2, ... in the order they appear
     in the first DPU's poc.yaml.
  4. Render + apply the GatewayClass (upstream Gateway-API v1) with the
     F5 CNE controllerName so FLO picks up Gateway objects that
     reference it.
  5. Wait for the CNEInstance to report Available.

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
	if err := requireTwoGates(f.yolo, "--confirm-deploy", f.confirmDeploy, p.Metadata.Name, "deploy cne"); err != nil {
		return err
	}
	if err := enforceValidateForPhase(out, p, repo, poc.PhaseDeploy, false); err != nil {
		return err
	}
	kubeconfig, err := requireKubeconfig(repo, "run `dpubnkctl cluster up` + `cluster join-dpus` + `deploy flo` first")
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "PoC:        %s\n", p.Metadata.Name)
	fmt.Fprintf(out, "Cluster:    %s\n\n", kubeconfig)

	r := &deploy.Runner{KubeconfigPath: kubeconfig, Out: prefixWriter{w: out, prefix: "      | "}}

	if !f.skipPull {
		fmt.Fprintln(out, "[1/6] Tools preflight ...")
		if err := r.CheckTools(ctx); err != nil {
			return err
		}
	}
	fmt.Fprintln(out, "      ok")

	// 2. License CR. Must come BEFORE CNEInstance — once FLO sees a
	//    CNEInstance it starts spinning up TMM pods, and TMM's bfd_watcher
	//    checks for F5SPKVlan config from CWC ~2 minutes after pod start.
	//    If the license isn't Active by then, CWC hasn't started pushing
	//    config, bfd_watcher logs "ERROR: vlan name not found", gives up,
	//    and the RoutingDone readiness gate stays False forever — CNEInstance
	//    never reaches Available. Verified on the wizard-deploy run, May 16.
	//
	//    New in 2.3: JWT no longer lives in FLO chart values; it goes into
	//    a License custom resource (k8s.f5net.com/v1) in the shared-component
	//    namespace. CWC watches the CR, validates the JWT, contacts the TEEM
	//    endpoint derived from the JWT's jku header (so prod vs tst is auto),
	//    and updates .status.state → PendingVerification → Active.
	//
	//    The License CRD is installed by FLO's crd-installer reconciliation,
	//    same two-step wait (for-create + Established) as F5SPKVlan below
	//    so the kubectl apply doesn't race the CRD's appearance.
	if err := applyLicenseCR(ctx, r, repo, p, f, out); err != nil {
		return err
	}

	// 3. Apply CNEInstance — FLO watches it and reconciles the downstream
	//    BNK CRs (TMM, dssm, observer, ...). Lands in `default` per the
	//    cne-instance.yaml.tmpl namespace (commit 0270d78).
	fmt.Fprintln(out, "[3/6] Rendering + applying CNEInstance ...")
	cne, err := deploy.RenderCNEInstance(p)
	if err != nil {
		return err
	}
	if err := saveAndApply(ctx, r, repo, "artifacts/cne-instance-rendered.yaml", cne); err != nil {
		return err
	}

	// 4. Apply F5SPKVlan resources. TMM's bfd_watcher needs these to come
	//    out of "ERROR: vlan name not found" and let the readiness gates
	//    (RoutingDone / ConfigurationDone) flip to True. Aggregator keys
	//    by Role+Tag so a single VLAN spanning both DPUs gets one
	//    F5SPKVlan with both selfip_v4s. Skip if the PoC has no DPU
	//    VLANs declared (single-host or NIC-mode topologies).
	//
	//    The F5SPKVlan CRD is installed by FLO's reconciliation of the
	//    CNEInstance we applied in step 3. That reconciliation isn't
	//    instant — without a wait, `kubectl apply` here loses a race
	//    with "no matches for kind F5SPKVlan in version k8s.f5net.com/v1
	//    ensure CRDs are installed first" (caught dogfooding e2e on
	//    lake1). Wait until the CRD is Established.
	fmt.Fprintln(out, "[4/6] Rendering + applying F5SPKVlan(s) ...")
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
		if err := applyWithWebhookRetry(ctx, r, repo, "artifacts/f5spkvlans-rendered.yaml",
			vlans, "f5validate.f5net.com", 30, 5*time.Second, out); err != nil {
			return fmt.Errorf("F5SPKVlan apply: %w", err)
		}
		fmt.Fprintf(out, "      applied %d aggregated F5SPKVlan(s).\n", vlanCount)
	} else {
		fmt.Fprintln(out, "      (no DPU VLANs in poc.yaml — skipped)")
	}

	// 5. Apply the upstream Gateway-API GatewayClass with the F5 CNE
	//    controllerName. FLO's f5-cne-controller registers itself under
	//    this name; once the GatewayClass exists, downstream Gateway
	//    objects can reference it via gatewayClassName. The historical
	//    BNKGatewayClassConfig CRD this used to also apply does not
	//    exist in BNK 2.2.0 — see AGENTS.md #20.
	fmt.Fprintln(out, "[5/6] Rendering + applying BNK GatewayClass ...")
	gwc, err := deploy.RenderGatewayClass("")
	if err != nil {
		return err
	}
	if err := saveAndApply(ctx, r, repo, "artifacts/bnk-gatewayclass-rendered.yaml", gwc); err != nil {
		return err
	}
	fmt.Fprintln(out, "      applied.")

	// 6. Wait for CNEInstance Available. The CNEInstance reports its
	//    overall readiness via the `Available` condition (NOT `Ready`
	//    — that condition name does not exist on CNEInstance). Available
	//    flips True when every component condition (F5Tmm, NodeLabeler,
	//    CRDInstaller, CNEController, Afm, Downloader, DSSM, Rabbitmq,
	//    CRDConversion, Cwc, IPAMController, Observer, OtelCollector,
	//    CSRC, …) reaches True. The CNEInstance lives in `default`
	//    (cne-instance.yaml.tmpl namespace), not f5-operators.
	//
	//    With the License applied first (step 2), TMM pods come up
	//    licensed; CWC pushes F5SPKVlan config; bfd_watcher succeeds on
	//    its first poll; RoutingDone flips True; Available follows.
	//    Without the reorder, this wait was the 15-min trap that left
	//    TMM stuck — see the comment in step 2.
	fmt.Fprintln(out, "[6/6] Waiting for CNEInstance Available ...")
	fmt.Fprintln(out, "      Requires Multus CNI + NADs in default (sf-external, sf-internal — installed by `deploy network`).")
	if err := r.Wait(ctx, "default", "Available",
		"cneinstance/bnk-instance", f.cneReadyTimeout); err != nil {
		// Hard error now that License runs before CNEInstance — if CNEInstance
		// doesn't flip Available, something deeper is wrong (license stuck
		// at PendingVerification, Multus DS broken, TMM pull-secret bad, ...)
		// and downstream phases can't proceed anyway. The prior WARN-only
		// behavior is what allowed the wizard-deploy May 16 run to report
		// "DONE" while TMM was in fact never Ready.
		return fmt.Errorf("CNEInstance not Available within %s — check `kubectl get cneinstance -A` and the per-component conditions in its .status. Common causes: license stuck (kubectl -n f5-cne-core get license), Multus DS not Ready (kubectl -n kube-system get ds kube-multus-ds), TMM image pull (kubectl -n default describe pod -l app=f5-tmm): %w", f.cneReadyTimeout, err)
	}
	fmt.Fprintln(out, "      CNEInstance Available.")

	p.Status.Deploy = "completed"
	p.Status.LastPhaseAt = time.Now().UTC()
	if err := savePoC(repo, p, out); err != nil {
		return err
	}
	appendDeployJournal(repo, p.Metadata.Name, "", "BNK DEPLOYED", "")
	fmt.Fprintln(out, "\nDONE. BNK platform deployed. `kubectl get pods -A` to inspect TMM, CNE controller, FLO, and license.")
	return nil
}

// applyLicenseCR applies the License CR and waits for Active — must run
// before the CNEInstance so TMM pods come up with CWC already pushing
// config. See the runDeployCNE step-2 comment for the failure mode this
// avoids. JWT-missing returns nil (matches the prior soft-skip behaviour
// in `deploy flo`, which already warns when the JWT path is absent).
func applyLicenseCR(ctx context.Context, r *deploy.Runner, repo string, p *poc.PoC, f *deployCNEFlags, out io.Writer) error {
	jwtPath := resolveRef(repo, p.BNK.JWTRef)
	if _, err := os.Stat(jwtPath); err != nil {
		fmt.Fprintf(out, "[2/6] License CR — JWT not at %s, skipping (deploy flo will have warned)\n", jwtPath)
		fmt.Fprintln(out, "      WARN: TMM will come up unlicensed; bfd_watcher will fail and CNEInstance will not reach Available.")
		return nil
	}
	jwt, err := readJWT(jwtPath)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "[2/6] Applying License CR (mode=%s) ...\n", f.licenseMode)
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
	// embeds the raw JWT. OpenFile so a prior run's file gets truncated
	// AND its mode reset to 0o600 — plain WriteFile keeps an existing
	// inode's permissions, which could leave a 0o644 JWT body readable
	// to other local users on the jumphost.
	licenseRendered := filepath.Join(repo, "artifacts", "license-cr-rendered.yaml")
	lf, err := os.OpenFile(licenseRendered, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := lf.WriteString(licenseYAML); err != nil {
		_ = lf.Close()
		return err
	}
	if err := lf.Close(); err != nil {
		return err
	}
	if err := os.Chmod(licenseRendered, 0o600); err != nil {
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
			// Proceed anyway — disconnected-mode flows expect the operator
			// to finish licensing out-of-band; failing the phase would
			// trap them. CNEInstance Available wait at step 6 will time
			// out clearly if TMM never gets configured.
			return nil
		}
		return fmt.Errorf("license did not reach Active within %s — `kubectl -n %s describe license %s`: %w",
			f.licenseReadyTimeout, deploy.SharedComponentNamespace, deploy.LicenseCRName, err)
	}
	fmt.Fprintln(out, "      License Active.")
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

// applyWithWebhookRetry wraps saveAndApply with a retry loop for the
// specific "validating webhook X: connection refused" race that
// happens when a Deployment hosting an admission webhook reports
// Available before the webhook server inside the pod has actually
// bound its port. Surfaces every other apply error immediately —
// this is not a generic retry, only the "webhook not bound yet" one.
//
// webhookName is matched against the kubectl error string ("the
// webhook \"<name>\"..."); attempts × interval bounds the total wait.
func applyWithWebhookRetry(ctx context.Context, r *deploy.Runner, repo, relPath, manifest, webhookName string, attempts int, interval time.Duration, out io.Writer) error {
	var applyErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		applyErr = saveAndApply(ctx, r, repo, relPath, manifest)
		if applyErr == nil {
			return nil
		}
		msg := applyErr.Error()
		if !strings.Contains(msg, webhookName) || !strings.Contains(msg, "connection refused") {
			return applyErr
		}
		fmt.Fprintf(out, "      webhook %s not bound yet, retrying in %s (attempt %d/%d) ...\n",
			webhookName, interval, attempt, attempts)
		time.Sleep(interval)
	}
	return fmt.Errorf("webhook %s never came up: %w", webhookName, applyErr)
}
