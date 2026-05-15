package version

// Build-time stamped values (see Makefile LDFLAGS).
var (
	Version    = "dev"
	Commit     = "none"
	BuildDate  = "unknown"
	BNKVersion = "2.3.0"
)

// Pinned defaults for BNK 2.3.0. These travel with the binary; a different
// dpubnkctl release targets a different BNK version. The FLO/CIS/cert-gen
// chart versions are NOT pinned here — they're resolved at deploy time
// from the f5-bigip-k8s-manifest release-manifest chart (see
// internal/deploy/manifest.go), which lists the exact set of charts and
// images that constitute the BNK release identified by CNEManifestVersion.
const (
	DOCAVersion = "3.2.0"
	BFBImage    = "bf-bundle-3.2.0-113_25.10_ubuntu-24.04_64k_prod.bfb"
	BFBBaseURL  = "https://content.mellanox.com/BlueField/BFBs/Ubuntu24.04"

	// K8sVersion is what we tell operators in the docs / cluster status.
	// K8sVersionPinned is what kubespray's `kube_version` accepts.
	//
	// BNK 2.3 release notes drop 1.32. Supported matrix: 1.30.10, 1.30.14,
	// 1.35.3. We default to 1.30.14 because kubespray v2.28.1 still
	// supports it (no kubespray bump needed). When 1.35 lands as the
	// default we'll bump kubespray separately.
	K8sVersion       = "1.30"
	K8sVersionPinned = "1.30.14"
	RuncVersion      = "1.2.1"
	ContainerdVer    = "1.7.23"
	PauseImageTag    = "3.10"
	DefaultDPUMTU    = 9000
	DefaultPodMTU    = 8900

	// Kubespray pins. v2.28.1 supports 1.30/1.31/1.32 and is the same
	// version we ran on the 2.2 branch — keep it until we have to move.
	KubesprayImage   = "quay.io/kubespray/kubespray:v2.28.1"
	KubesprayVersion = "v2.28.1"

	// kubectl + helm — used during Phase 4 (BNK deploy). alpine/k8s
	// bundles both with stable versioned tags. Bumped to match
	// K8sVersionPinned so `kubectl` server-version skew stays in the
	// supported ±1 minor window.
	K8sToolsImage = "alpine/k8s:1.30.14"

	// Cert-manager — required dependency for FLO + CWC. Version remains
	// independent of the release manifest (jetstack repo, not F5's OCI).
	CertManagerChart   = "cert-manager"
	CertManagerRepo    = "https://charts.jetstack.io"
	CertManagerVersion = "v1.16.2"

	// Release manifest — the F5 bill-of-materials chart that pins the
	// FLO, CIS, cert-gen, and image versions for this BNK release. Pull
	// it at deploy time; do NOT hardcode the FLO chart version here.
	ReleaseManifestRepo  = "oci://repo.f5.com/release"
	ReleaseManifestChart = "f5-bigip-k8s-manifest"

	// CNEManifestVersion is the version coordinate inside the release
	// manifest. CNEInstance.spec.manifestVersion references it directly;
	// PullReleaseManifest uses it as the --version arg to helm pull.
	CNEManifestVersion = "2.3.0-3.2598.3-0.0.170"

	// FARRegistryHost is the OCI registry hostname for all F5-published
	// charts and images. The release manifest itself, FLO, CIS, and
	// cert-gen all live here under different path prefixes.
	FARRegistryHost = "repo.f5.com"

	// FLOChartOCIRef is the full OCI reference for the FLO chart. The
	// version is resolved at deploy time from the release-manifest chart
	// (see internal/deploy/manifest.go), so this constant only encodes
	// the path. Helm's `--version` flag carries the resolved version.
	FLOChartOCIRef = "oci://repo.f5.com/charts/f5-lifecycle-operator"
)
