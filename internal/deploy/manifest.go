package deploy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/mwiget/dpubnkctl/internal/version"
)

// ReleaseManifest is the parsed contents of the f5-bigip-k8s-manifest
// release-manifest chart. F5 ships one manifest per BNK release; it pins
// the exact chart and image versions that constitute that release.
//
// Example (truncated):
//
//	f5_helm_repo: oci://repo.f5.com
//	f5_docker_repo: repo.f5.com
//	releases:
//	  - version: 2.3.0-3.2598.3-0.0.170
//	    helm_charts:
//	      - {name: charts/f5-lifecycle-operator, version: v2.10.x-…}
//	      - {name: utils/f5-cert-gen,            version: 0.9.x}
//	    docker_images:
//	      - {name: images/cert-manager-controller, version: v2.5.x}
//
// We expose chart and image versions as maps keyed by the slash-separated
// name. Callers ask: `m.Chart("charts/f5-lifecycle-operator")`.
type ReleaseManifest struct {
	HelmRepo    string            // oci://repo.f5.com — copied from f5_helm_repo
	DockerRepo  string            // repo.f5.com — copied from f5_docker_repo
	Version     string            // releases[0].version
	HelmCharts  map[string]string // chart name → version
	DockerImgs  map[string]string // image name → version
	rawManifest []byte            // for diagnostics / artifacts/ persistence
}

// rawReleaseManifest mirrors the on-disk YAML shape. We parse into this
// then flatten into ReleaseManifest.
type rawReleaseManifest struct {
	F5HelmRepo   string `yaml:"f5_helm_repo"`
	F5DockerRepo string `yaml:"f5_docker_repo"`
	Releases     []struct {
		Version    string `yaml:"version"`
		HelmCharts []struct {
			Name    string `yaml:"name"`
			Version string `yaml:"version"`
		} `yaml:"helm_charts"`
		DockerImages []struct {
			Name    string `yaml:"name"`
			Version string `yaml:"version"`
		} `yaml:"docker_images"`
	} `yaml:"releases"`
}

// ParseReleaseManifest decodes a release-manifest YAML body. Exported so
// `release-manifest pull` and any future tests can run it on an already-
// downloaded file without going through helm.
func ParseReleaseManifest(body []byte) (*ReleaseManifest, error) {
	var raw rawReleaseManifest
	if err := yaml.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("parse release-manifest yaml: %w", err)
	}
	if len(raw.Releases) == 0 {
		return nil, fmt.Errorf("release-manifest has no `releases[]` entries")
	}
	rel := raw.Releases[0]
	if rel.Version == "" {
		return nil, fmt.Errorf("release-manifest releases[0].version is empty")
	}
	m := &ReleaseManifest{
		HelmRepo:    raw.F5HelmRepo,
		DockerRepo:  raw.F5DockerRepo,
		Version:     rel.Version,
		HelmCharts:  make(map[string]string, len(rel.HelmCharts)),
		DockerImgs:  make(map[string]string, len(rel.DockerImages)),
		rawManifest: body,
	}
	for _, c := range rel.HelmCharts {
		if c.Name != "" {
			m.HelmCharts[c.Name] = c.Version
		}
	}
	for _, i := range rel.DockerImages {
		if i.Name != "" {
			m.DockerImgs[i.Name] = i.Version
		}
	}
	return m, nil
}

// Chart returns the resolved version for a chart name. Returns an empty
// string if the chart isn't listed (let callers decide what to do —
// some charts are optional, others should fail loudly).
func (m *ReleaseManifest) Chart(name string) string {
	return m.HelmCharts[name]
}

// Image returns the resolved version for an image name.
func (m *ReleaseManifest) Image(name string) string {
	return m.DockerImgs[name]
}

// RawYAML returns the original YAML bytes — useful for persisting the
// resolved manifest under artifacts/ for SE review and to keep a
// per-deploy audit trail.
func (m *ReleaseManifest) RawYAML() []byte { return m.rawManifest }

// PullReleaseManifest authenticates to repo.f5.com with FAR credentials,
// pulls the f5-bigip-k8s-manifest chart at the requested version, and
// returns the parsed manifest. The pulled tgz and extracted YAML are
// written under cacheDir for caching + audit.
//
// Implementation: shell to the alpine/k8s container so we don't depend
// on the operator having helm on PATH. The container's network=host
// lets it reach repo.f5.com through whatever route the operator has
// (mgmt-route in lab setups — see AGENTS.md #23).
//
// auth is the FAR registry credentials. For F5 GAR, Username is
// "_json_key" and Password is the raw GCP service-account JSON
// (see internal/deploy/license.go::UnwrapGARAuth for extraction from
// the FAR tgz).
func PullReleaseManifest(ctx context.Context, auth OCIAuth, manifestVersion, cacheDir string) (*ReleaseManifest, error) {
	if manifestVersion == "" {
		manifestVersion = version.GetCNEManifestVersion()
		manifestVersion = version.GetCNEManifestVersion()
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir cache %s: %w", cacheDir, err)
	}
	absCache, err := filepath.Abs(cacheDir)
	if err != nil {
		return nil, err
	}

	// Inside the container: log in, helm-pull into /work, untar, cat the
	// manifest YAML to stdout. The host parses stdout. The tgz + extracted
	// dir stay in /work (mapped to absCache) so the operator can inspect.
	//
	// Helm pull adds a `f5-bigip-k8s-manifest-<ver>.tgz` and extracts to
	// `f5-bigip-k8s-manifest-<ver>/`. The inner manifest YAML is at
	// `bigip-k8s-manifest-<ver>.yaml` (note: chart name doubled, hyphenated
	// once and prefixed once — that's F5's packaging quirk, see
	// IBM ROKS terraform modules/roks-cluster-install-flo/modules/flo/main.tf
	// line ~438).
	// Username + manifestVersion are passed as docker env vars rather
	// than spliced into the script body. Today auth.Username is the
	// constant "_json_key" and manifestVersion comes from the binary-
	// pinned version.CNEManifestVersion, but both surfaces are
	// poc.yaml-ready in the medium term. Mirror of the fix in
	// PullF5CertGen (commit 0e63fa1 / review S-M6) — same threat model:
	// --network=host alpine/k8s container running as root inside docker
	// means an injection here is consequential.
	script := "set -e\n" +
// ...existing code...
		"cat | helm registry login " + version.GetFARRegistryHost() + " --username \"$USERNAME\" --password-stdin >/dev/null\n" +
		"cd /work\n" +
		"rm -f \"f5-bigip-k8s-manifest-${MANIFEST_VERSION}.tgz\"\n" +
		"rm -rf \"f5-bigip-k8s-manifest-${MANIFEST_VERSION}\"\n" +
		"helm pull " + version.GetReleaseManifestRepo() + "/" + version.GetReleaseManifestChart() + " --version \"$MANIFEST_VERSION\" -d . >/dev/null\n" +
		"tar -xzf \"f5-bigip-k8s-manifest-${MANIFEST_VERSION}.tgz\"\n" +
		"echo '---DPUBNKCTL-MANIFEST-BEGIN---'\n" +
		"cat \"f5-bigip-k8s-manifest-${MANIFEST_VERSION}/bigip-k8s-manifest-${MANIFEST_VERSION}.yaml\"\n" +
		"echo '---DPUBNKCTL-MANIFEST-END---'\n"

	dockerArgs := []string{
		"run", "--rm", "-i",
		"-v", absCache + ":/work",
		"--network=host",
		"-e", "USERNAME=" + auth.Username,
		"-e", "MANIFEST_VERSION=" + manifestVersion,
		version.K8sToolsImage,
		"sh", "-c", script,
	}
	cmd := exec.CommandContext(ctx, "docker", dockerArgs...)
	cmd.Stdin = strings.NewReader(auth.Password + "\n")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("helm pull release-manifest %s: %w\n%s",
			manifestVersion, err, strings.TrimSpace(stderr.String()))
	}
	body, err := extractManifestBody(stdout.Bytes())
	if err != nil {
		return nil, fmt.Errorf("read manifest body from helm output: %w\n%s",
			err, stderr.String())
	}
	m, err := ParseReleaseManifest(body)
	if err != nil {
		return nil, err
	}
	// Persist the parsed manifest under cacheDir/manifest.yaml so the rest
	// of the binary (and operators) can find it without re-running helm.
	if err := os.WriteFile(filepath.Join(absCache, "manifest.yaml"), body, 0o644); err != nil {
		return nil, fmt.Errorf("write manifest cache: %w", err)
	}
	return m, nil
}

// extractManifestBody slices out the lines between the BEGIN/END markers
// the in-container script emits. helm chatter (like login warnings,
// progress) before BEGIN and any container trailer after END is dropped.
func extractManifestBody(stdout []byte) ([]byte, error) {
	const begin = "---DPUBNKCTL-MANIFEST-BEGIN---"
	const end = "---DPUBNKCTL-MANIFEST-END---"
	bi := bytes.Index(stdout, []byte(begin))
	if bi < 0 {
		return nil, fmt.Errorf("BEGIN marker not found")
	}
	ei := bytes.Index(stdout, []byte(end))
	if ei < 0 || ei < bi {
		return nil, fmt.Errorf("END marker not found")
	}
	// Skip the marker line itself + its trailing newline.
	body := stdout[bi+len(begin) : ei]
	return bytes.TrimSpace(body), nil
}

// ExtractFARRegistryAuth is the small bridge between the FAR tgz on
// disk and a deploy.OCIAuth ready for helm registry login. Reads the
// FAR tgz, pulls the raw GCP service-account JSON, and returns auth
// suitable for `helm registry login -u _json_key --password-stdin`.
func ExtractFARRegistryAuth(farTgzPath string) (OCIAuth, error) {
	docker, err := ExtractFARDockerConfig(farTgzPath)
	if err != nil {
		return OCIAuth{}, err
	}
	saJSON, err := UnwrapGARAuth(docker)
	if err != nil {
		return OCIAuth{}, err
	}
	return OCIAuth{
		RegistryHost: version.GetFARRegistryHost(),
		Username:     "_json_key",
		Password:     saJSON,
	}, nil
}

// SinkSummary writes a one-line-per-chart summary to w, sorted for
// stable output. Useful for operator banners and audit logs.
func (m *ReleaseManifest) SinkSummary(w io.Writer) {
	fmt.Fprintf(w, "Release-manifest:  %s\n", m.Version)
	fmt.Fprintf(w, "  helm repo:       %s\n", m.HelmRepo)
	fmt.Fprintf(w, "  docker repo:     %s\n", m.DockerRepo)
	fmt.Fprintf(w, "  helm charts:     %d\n", len(m.HelmCharts))
	fmt.Fprintf(w, "  docker images:   %d\n", len(m.DockerImgs))
	// Highlight the charts we actually use.
	for _, name := range []string{
		"charts/f5-lifecycle-operator",
		"utils/f5-cert-gen",
		"charts/cwc",
		"charts/f5-cert-manager",
	} {
		if v := m.Chart(name); v != "" {
			fmt.Fprintf(w, "    %-35s %s\n", name, v)
		}
	}
}
