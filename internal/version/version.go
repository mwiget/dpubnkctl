package version

// Build-time stamped values (see Makefile LDFLAGS).
var (
	Version    = "dev"
	Commit     = "none"
	BuildDate  = "unknown"
	BNKVersion = "2.2.0"
)

// Pinned defaults for BNK 2.2.0. These travel with the binary; a different
// dpubnkctl release targets a different BNK version.
const (
	DOCAVersion    = "2.9.2"
	BFBImage       = "bf-bundle-2.9.2-32_25.02_ubuntu-22.04_prod.bfb"
	BFBBaseURL     = "https://content.mellanox.com/BlueField/BFBs/Ubuntu22.04"
	FLOChartVer    = "v2.9.27-0.2.10"
	FLOChartRef    = "oci://repo.f5.com/charts/f5-lifecycle-operator"
	K8sVersion       = "1.32"
	// K8sVersionPinned matches kubespray v2.28.1's default (which only
	// supports 1.30/1.31/1.32). No leading "v" — kubespray rejects it.
	K8sVersionPinned = "1.32.8"
	RuncVersion      = "1.2.1"
	ContainerdVer    = "1.7.23"
	PauseImageTag    = "3.10"
	DefaultDPUMTU    = 9000
	DefaultPodMTU    = 8900

	// Kubespray pins — match f5-bnk-nvidia-bf3-installations v2.2.0-static.
	KubesprayImage   = "quay.io/kubespray/kubespray:v2.28.1"
	KubesprayVersion = "v2.28.1"

	// kubectl + helm — used during Phase 4 (BNK deploy). alpine/k8s
	// bundles both with stable versioned tags (bitnami/kubectl removed
	// versioned tags in late 2024). One image keeps `docker pull`
	// surface small.
	K8sToolsImage = "alpine/k8s:1.32.8"

	// Cert-manager — required dependency for the FLO chart's webhook.
	CertManagerChart   = "cert-manager"
	CertManagerRepo    = "https://charts.jetstack.io"
	CertManagerVersion = "v1.16.2"
)
