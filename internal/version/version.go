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
var (
	DOCAVersion = "3.2.0"
	BFBImage    = "bf-bundle-3.2.0-113_25.10_ubuntu-24.04_64k_prod.bfb"
	BFBBaseURL  = "https://content.mellanox.com/BlueField/BFBs/Ubuntu24.04"

	BFBImageSHA256 = ""

	K8sVersion       = "1.30"
	K8sVersionPinned = "1.30.14"
	RuncVersion      = "1.2.1"
	ContainerdVer    = "1.7.23"
	PauseImageTag    = "3.10"
	DefaultDPUMTU    = 9000
	DefaultPodMTU    = 8900

	KubesprayImage   = "quay.io/kubespray/kubespray:v2.28.1"
	KubesprayVersion = "v2.28.1"

	K8sToolsImage = "alpine/k8s:1.31.5"

	CertManagerChart   = "cert-manager"
	CertManagerRepo    = "https://charts.jetstack.io"
	CertManagerVersion = "v1.16.2"

	releaseManifestChart = "f5-bigip-k8s-manifest"
	cneManifestVersion   = "2.3.0-3.2598.3-0.0.170"

	devRepo = false
)

// SetDevRepo toggles the dev repo flag for registry/chart helpers
func SetDevRepo(dev bool) {
       devRepo = dev
}

// Helper functions for callers
func GetReleaseManifestRepo() string {
	if devRepo {
		 return "oci://devrepo.f5.com/release"
	}
	return "oci://repo.f5.com/release"
}

func GetFARRegistryHost() string {
	if devRepo {
		 return "devrepo.f5.com"
	}
	return "repo.f5.com"
}

func GetFLOChartOCIRef() string {
	if devRepo {
		 return "oci://devrepo.f5.com/charts/f5-lifecycle-operator"
	}
	return "oci://repo.f5.com/charts/f5-lifecycle-operator"
}

func GetReleaseManifestChart() string {
	return releaseManifestChart
}

func GetCNEManifestVersion() string {
	return cneManifestVersion
}
