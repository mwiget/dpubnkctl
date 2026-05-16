package deploy

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/mwiget/dpubnkctl/internal/version"
)

// SharedComponentNamespace is the canonical namespace name where the F5
// Cluster-Wide Controller (CWC), License CR, observer, and other
// release-wide singletons live in BNK 2.3+. F5's public docs use
// `f5-cne-core`; this constant centralizes it so a future override
// flag has one place to change.
const SharedComponentNamespace = "f5-cne-core"

// PullF5CertGen pulls the f5-cert-gen helm chart from repo.f5.com at the
// version specified by the resolved release manifest. The extracted
// chart directory contains gen_cert.sh which generates the API-server
// TLS material the CWC requires.
//
// destDir is where the chart tarball + extracted `cert-gen/` tree live.
// Both are kept on disk for audit + so a subsequent failure can be
// debugged without re-pulling.
func PullF5CertGen(ctx context.Context, auth OCIAuth, chartVersion, destDir string) error {
	if chartVersion == "" {
		return fmt.Errorf("PullF5CertGen: chartVersion required (resolve from release-manifest first)")
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}
	absDest, err := filepath.Abs(destDir)
	if err != nil {
		return err
	}
	// Username + chartVersion are passed as env vars rather than spliced
	// into the shell string so an attacker-controlled value (today
	// FARRegistryAuth.Username is hardcoded to "_json_key" and
	// chartVersion comes from a TLS-protected release manifest, but the
	// surface is poc.yaml-ready in both cases) can't escape a quote and
	// land arbitrary commands in the sh -c invocation. Inside the
	// script, $USERNAME / $CHART_VERSION expand once at sh parse time —
	// they're already inside the shell, no further quoting needed.
	const script = `set -e
cat | helm registry login ` + version.FARRegistryHost + ` --username "$USERNAME" --password-stdin >/dev/null
cd /work
rm -f "f5-cert-gen-${CHART_VERSION}.tgz"
rm -rf cert-gen
helm pull oci://` + version.FARRegistryHost + `/utils/f5-cert-gen --version "$CHART_VERSION" -d . >/dev/null
tar -xzf "f5-cert-gen-${CHART_VERSION}.tgz"
`
	cmd := exec.CommandContext(ctx, "docker",
		"run", "--rm", "-i",
		"-v", absDest+":/work",
		"--network=host",
		"-e", "USERNAME="+auth.Username,
		"-e", "CHART_VERSION="+chartVersion,
		version.K8sToolsImage,
		"sh", "-c", script)
	cmd.Stdin = strings.NewReader(auth.Password + "\n")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("helm pull f5-cert-gen %s: %w\n%s", chartVersion, err, strings.TrimSpace(string(out)))
	}
	// Sanity-check the expected file landed on the host side.
	genCert := filepath.Join(absDest, "cert-gen", "gen_cert.sh")
	if _, err := os.Stat(genCert); err != nil {
		return fmt.Errorf("f5-cert-gen chart pulled but %s missing", genCert)
	}
	return nil
}

// GenerateCWCCerts runs cert-gen/gen_cert.sh inside the alpine/k8s
// container to produce cwc-license-certs.yaml + cwc-license-client-certs.yaml
// in workDir. The script needs `make` and `openssl` — both ship in
// alpine/k8s, but make has to be installed via apk (the image trims
// build tools). We apk-add it at the top of the script.
//
// The SAN passed to gen_cert.sh is the FQDN the CWC service answers on:
// `f5-spk-cwc.<utils-ns>.svc.cluster.local`. Defaults to
// SharedComponentNamespace.
func GenerateCWCCerts(ctx context.Context, workDir, utilsNamespace string, out io.Writer) error {
	if utilsNamespace == "" {
		utilsNamespace = SharedComponentNamespace
	}
	abs, err := filepath.Abs(workDir)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(abs, "cert-gen", "gen_cert.sh")); err != nil {
		return fmt.Errorf("cert-gen/gen_cert.sh missing in %s — call PullF5CertGen first", abs)
	}
	san := fmt.Sprintf("f5-spk-cwc.%s.svc.cluster.local", utilsNamespace)
	script := strings.Join([]string{
		`set -e`,
		// gen_cert.sh shells out to `make` + python3; alpine/k8s ships
		// neither. Add them. Pin nothing — we want the apk repo's pick
		// to track the alpine release we're already on.
		`apk add --no-cache make openssl python3 >/dev/null`,
		`cd /work`,
		// Idempotent: delete the previous run's artifacts so re-run works.
		`rm -rf api-server-secrets cwc-license-certs.yaml cwc-license-client-certs.yaml`,
		`sh cert-gen/gen_cert.sh -s=api-server -a=` + san + ` -n=1`,
	}, " && ")
	cmd := exec.CommandContext(ctx, "docker",
		"run", "--rm",
		"-v", abs+":/work",
		"--network=host",
		version.K8sToolsImage,
		"sh", "-c", script)
	if out != nil {
		cmd.Stdout = out
		cmd.Stderr = out
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("gen_cert.sh -s=api-server -a=%s: %w", san, err)
	}
	for _, want := range []string{"cwc-license-certs.yaml", "cwc-license-client-certs.yaml"} {
		if _, err := os.Stat(filepath.Join(abs, want)); err != nil {
			return fmt.Errorf("gen_cert.sh did not produce %s in %s", want, abs)
		}
	}
	return nil
}

// ApplyCWCCerts kubectl-applies the two manifests produced by
// GenerateCWCCerts into the shared-component namespace.
//
// gen_cert.sh emits manifests without a metadata.namespace field (and
// with 1-space indentation that doesn't play nicely with naive YAML
// patches). Instead of rewriting the body, pass the namespace to
// kubectl via `-n` — kubectl honors that for any namespace-scoped
// resource whose metadata.namespace is unset.
func ApplyCWCCerts(ctx context.Context, r *Runner, workDir, namespace string) error {
	if namespace == "" {
		namespace = SharedComponentNamespace
	}
	if err := r.Apply(ctx, RenderNamespace(namespace)); err != nil {
		return fmt.Errorf("create namespace %s: %w", namespace, err)
	}
	for _, name := range []string{"cwc-license-certs.yaml", "cwc-license-client-certs.yaml"} {
		body, err := os.ReadFile(filepath.Join(workDir, name))
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		if err := r.ApplyInNamespace(ctx, namespace, string(body)); err != nil {
			return fmt.Errorf("apply %s: %w", name, err)
		}
	}
	return nil
}

// injectNamespace rewrites the "metadata:" block of each YAML doc in
// body to include `namespace: <ns>` if it isn't already present. Cheap
// line-oriented patch — assumes well-formed YAML with metadata indented
// by 2 spaces (which gen_cert.sh produces). Not a general-purpose YAML
// patcher; reads only the two files this helper generates.
func injectNamespace(body []byte, ns string) []byte {
	lines := strings.Split(string(body), "\n")
	out := make([]string, 0, len(lines)+8)
	inMeta := false
	hasNS := false
	flushMeta := func() {
		if inMeta && !hasNS {
			out = append(out, "  namespace: "+ns)
		}
		inMeta = false
		hasNS = false
	}
	for _, l := range lines {
		switch {
		case strings.HasPrefix(l, "---") || strings.HasPrefix(l, "apiVersion:") || strings.HasPrefix(l, "kind:"):
			flushMeta()
			out = append(out, l)
		case strings.HasPrefix(l, "metadata:"):
			flushMeta()
			inMeta = true
			out = append(out, l)
		case inMeta && strings.HasPrefix(l, "  namespace:"):
			hasNS = true
			out = append(out, l)
		case inMeta && (strings.HasPrefix(l, "  ") || l == ""):
			out = append(out, l)
		default:
			flushMeta()
			out = append(out, l)
		}
	}
	flushMeta()
	return []byte(strings.Join(out, "\n"))
}

// PullAndApplyCWCCerts is the convenience one-shot used by `deploy flo`:
// pull f5-cert-gen, generate the certs, apply them to the cluster.
// workDir defaults to <poc>/artifacts/cwc-certs/.
func PullAndApplyCWCCerts(ctx context.Context, r *Runner, auth OCIAuth, certGenChartVersion, workDir, namespace string, out io.Writer) error {
	if err := PullF5CertGen(ctx, auth, certGenChartVersion, workDir); err != nil {
		return err
	}
	if err := GenerateCWCCerts(ctx, workDir, namespace, out); err != nil {
		return err
	}
	return ApplyCWCCerts(ctx, r, workDir, namespace)
}
