package deploy

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"text/template"
	"time"

	"github.com/mwiget/dpubnkctl/internal/embedded"
)

// LicenseCRName is the canonical name of the cluster-wide license CR
// per F5's 2.3 docs ("f5-cne-cluster-license"). The IBM ROKS terraform
// uses "bnk-license" — they're functionally equivalent. We pick the
// public-docs name so operators following along can `kubectl get
// license.k8s.f5net.com f5-cne-cluster-license` straight from the docs
// without renaming anything.
const LicenseCRName = "f5-cne-cluster-license"

// LicenseInputs are substituted into license-cr.yaml.tmpl.
type LicenseInputs struct {
	Name          string // defaults to LicenseCRName
	Namespace     string // defaults to SharedComponentNamespace
	OperationMode string // "connected" | "disconnected"
	JWT           string // raw JWT (single line, no quotes)
}

// RenderLicenseCR substitutes the embedded license-cr template. The JWT
// goes onto a single YAML scalar line — gen_cert.sh / kubectl-apply
// pipelines balk on multi-line / wrapped JWTs.
func RenderLicenseCR(in LicenseInputs) (string, error) {
	if in.Name == "" {
		in.Name = LicenseCRName
	}
	if in.Namespace == "" {
		in.Namespace = SharedComponentNamespace
	}
	if in.OperationMode == "" {
		in.OperationMode = "connected"
	}
	if strings.ContainsAny(in.JWT, "\r\n") {
		return "", fmt.Errorf("JWT contains newlines (got %d bytes); paste as a single line", len(in.JWT))
	}
	raw, err := embedded.Templates.ReadFile("templates/license-cr.yaml.tmpl")
	if err != nil {
		return "", err
	}
	t, err := template.New("license-cr").Parse(string(raw))
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	if err := t.Execute(&b, in); err != nil {
		return "", err
	}
	return b.String(), nil
}

// WaitForLicenseActive polls `kubectl get license.k8s.f5net.com <name>
// -o jsonpath={.status.state}` until it returns Active, or the timeout
// fires. F5 ships a "STATE" printer column that walks through
// PendingVerification → Active (connected mode), or stays at
// PendingVerification until the operator runs the disconnected-mode
// curl ritual. We accept Active as the only success state — the
// caller should fall through gracefully (warn, not error) for
// PendingVerification because disconnected-mode customers need that.
func WaitForLicenseActive(ctx context.Context, r *Runner, name, namespace string, timeout time.Duration) error {
	if name == "" {
		name = LicenseCRName
	}
	if namespace == "" {
		namespace = SharedComponentNamespace
	}
	deadline := time.Now().Add(timeout)
	for {
		out, err := r.KubectlCapture(ctx, "-n", namespace, "get",
			"license.k8s.f5net.com", name,
			"-o", "jsonpath={.status.state}")
		state := strings.TrimSpace(out)
		switch state {
		case "Active":
			return nil
		case "Registering":
			// CWC is talking to F5's licensing server to register the
			// cluster's digital asset. First-time registration on a
			// connected-mode cluster — takes 5-15 minutes. Keep polling.
			if time.Now().After(deadline) {
				return fmt.Errorf("license %s/%s stuck at Registering after %s (CWC could not complete device registration; check `kubectl -n %s describe license %s` for the CWC error)",
					namespace, name, timeout, namespace, name)
			}
		case "PendingVerification":
			// disconnected-mode customers stay here forever until they
			// run the manual curl-the-licensing-server ritual. Bubble
			// up as a typed sentinel so the caller can downgrade to a
			// warning.
			if time.Now().After(deadline) {
				return ErrLicensePendingVerification
			}
		case "":
			// Status not yet populated by CWC — keep polling.
			if err != nil && !strings.Contains(err.Error(), "NotFound") {
				return fmt.Errorf("kubectl get license %s/%s: %w", namespace, name, err)
			}
		default:
			return fmt.Errorf("license %s/%s reached unexpected state %q (expected Active)",
				namespace, name, state)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("license %s/%s did not reach Active within %s (last state %q)",
				namespace, name, timeout, state)
		}
		time.Sleep(5 * time.Second)
	}
}

// ErrLicensePendingVerification signals the disconnected-mode case
// where the License CR exists, was accepted by CWC, but is awaiting
// the operator's manual licensing-server registration call. Callers
// should typically log + continue rather than fail the deploy.
var ErrLicensePendingVerification = fmt.Errorf("license stuck at PendingVerification (disconnected-mode operator action required)")
