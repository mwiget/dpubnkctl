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

	// BFBImageSHA256 — when non-empty, EnsureBFB verifies the downloaded
	// file matches this hex digest before installing. Empty means
	// integrity is not pinned and the download is trust-on-first-use;
	// the file is still served over TLS so passive MITM is mitigated,
	// but content substitution by the origin (or a poc.yaml-overridden
	// bfb_url) is silent. Populate once the BFB's published checksum
	// is confirmed against the upstream NVIDIA release notes — leave
	// empty here pending that confirmation.
	BFBImageSHA256 = ""

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
	// bundles both with stable versioned tags. Set ONE minor ahead of
	// K8sVersionPinned for two reasons:
	//   1. `kubectl wait --for=create` (used by deploy_cne to wait for
	//      F5SPKVlan + License CRDs to appear) was added in kubectl
	//      1.31. We don't want to push the server to 1.31 yet (BNK 2.3
	//      release notes name 1.30.x as supported), but the kubectl
	//      side can move forward — Kubernetes supports ±1 minor skew
	//      between kubectl and the apiserver.
	//   2. Newer kubectl handles deb822 apt source warnings + a few
	//      `kubectl apply` server-side-apply edge cases better.
	K8sToolsImage = "alpine/k8s:1.31.5"

	// Calico via tigera-operator — deployed after cluster-up, replacing
	// kubespray's built-in calico (which doesn't work reliably on DPUs).
	CalicoVersion            = "v3.29.1"
	TigeraOperatorVersion    = "v1.36.2"
	TigeraOperatorManifest   = "https://raw.githubusercontent.com/projectcalico/calico/" + CalicoVersion + "/manifests/tigera-operator.yaml"

	// NFS CSI driver — required for BNK (replaces local-path-provisioner).
	NFSCSIDriverVersion  = "v4.13.4"
	NFSCSIChartRepo      = "https://raw.githubusercontent.com/kubernetes-csi/csi-driver-nfs/master/charts"
	NFSCSIChartName      = "csi-driver-nfs"

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

// Airgap image and file lists. These travel with the binary and are
// bumped whenever the version pins above change. Derived from
// kubespray v2.28.1 defaults for K8s 1.30 + manually verified images
// from the embedded network manifests.

var AirgapKubesprayImages = []string{
	"registry.k8s.io/kube-apiserver:v" + K8sVersionPinned,
	"registry.k8s.io/kube-controller-manager:v" + K8sVersionPinned,
	"registry.k8s.io/kube-scheduler:v" + K8sVersionPinned,
	"registry.k8s.io/kube-proxy:v" + K8sVersionPinned,
	"registry.k8s.io/coredns/coredns:v1.11.3",
	"registry.k8s.io/pause:3.9",
	"registry.k8s.io/pause:" + PauseImageTag,
	"registry.k8s.io/dns/k8s-dns-node-cache:1.25.0",
	"registry.k8s.io/cpa/cluster-proportional-autoscaler:v1.8.8",
	"quay.io/coreos/etcd:v3.5.22",
	"docker.io/library/nginx:1.27.4-alpine",
	"docker.io/library/haproxy:3.1.3-alpine",
	"docker.io/library/registry:2.8.1",
}

var AirgapNetworkImages = []string{
	"ghcr.io/k8snetworkplumbingwg/multus-cni:snapshot-thick",
	"ghcr.io/k8snetworkplumbingwg/sriov-network-device-plugin:latest",
	"quay.io/tigera/operator:" + TigeraOperatorVersion,
	"docker.io/calico/apiserver:" + CalicoVersion,
	"docker.io/calico/cni:" + CalicoVersion,
	"docker.io/calico/csi:" + CalicoVersion,
	"docker.io/calico/kube-controllers:" + CalicoVersion,
	"docker.io/calico/node-driver-registrar:" + CalicoVersion,
	"docker.io/calico/node:" + CalicoVersion,
	"docker.io/calico/pod2daemon-flexvol:" + CalicoVersion,
	"docker.io/calico/typha:" + CalicoVersion,
}

var AirgapNFSImages = []string{
	"registry.k8s.io/sig-storage/csi-node-driver-registrar:v2.17.0",
	"registry.k8s.io/sig-storage/csi-provisioner:v6.3.0",
	"registry.k8s.io/sig-storage/csi-resizer:v2.2.0",
	"registry.k8s.io/sig-storage/csi-snapshotter:v8.6.0",
	"registry.k8s.io/sig-storage/livenessprobe:v2.19.0",
	"registry.k8s.io/sig-storage/nfsplugin:" + NFSCSIDriverVersion,
}

var AirgapCertManagerImages = []string{
	"quay.io/jetstack/cert-manager-controller:" + CertManagerVersion,
	"quay.io/jetstack/cert-manager-webhook:" + CertManagerVersion,
	"quay.io/jetstack/cert-manager-cainjector:" + CertManagerVersion,
	"quay.io/jetstack/cert-manager-startupapicheck:" + CertManagerVersion,
}

var AirgapBNKHostImages = []string{
	"repo.f5.com/images/crd-conversion:v1.250.3",
	"repo.f5.com/images/crd-installer:v14.59.1-0.0.70",
	"repo.f5.com/images/crdupdater:v0.45.3-0.0.2",
	"repo.f5.com/images/f5-cert-client:v3.6.6",
	"repo.f5.com/images/f5-coremond:v0.16.2",
	"repo.f5.com/images/f5-csm-qkview:v0.14.0",
	"repo.f5.com/images/f5-downloader:v0.32.11-0.0.5",
	"repo.f5.com/images/f5-dssm-store:v5.1.49-0.0.3",
	"repo.f5.com/images/f5-fluentbit:v1.5.2",
	"repo.f5.com/images/f5-fluentd:v2.5.0-0.0.4",
	"repo.f5.com/images/f5ingress:v14.59.1-0.0.70",
	"repo.f5.com/images/f5ing-tmm-pod-manager:v1.6.1-0.0.4",
	"repo.f5.com/images/f5-ipam-controller:v1.5.2-0.0.7",
	"repo.f5.com/images/f5-l4p-engine:v1.130.9-0.0.2",
	"repo.f5.com/images/f5-license-helper:v0.15.1-0.0.2",
	"repo.f5.com/images/f5-lifecycle-operator:v2.21.13-0.0.28",
	"repo.f5.com/images/f5-toda-observer:v5.30.13-0.0.5",
	"repo.f5.com/images/opentelemetry-collector-contrib:0.149.0",
	"repo.f5.com/images/rabbit:v0.6.2",
	"repo.f5.com/images/spk-csrc:v0.9.7-0.0.2",
	"repo.f5.com/images/spk-cwc:v0.41.3-0.0.5",
}

var AirgapDPUKubesprayImages = []string{
	"registry.k8s.io/kube-proxy:v" + K8sVersionPinned,
	"registry.k8s.io/pause:" + PauseImageTag,
	"registry.k8s.io/pause:3.9",
	"registry.k8s.io/pause:3.8",
	"registry.k8s.io/dns/k8s-dns-node-cache:1.25.0",
	"docker.io/calico/apiserver:" + CalicoVersion,
	"docker.io/calico/cni:" + CalicoVersion,
	"docker.io/calico/csi:" + CalicoVersion,
	"docker.io/calico/kube-controllers:" + CalicoVersion,
	"docker.io/calico/node-driver-registrar:" + CalicoVersion,
	"docker.io/calico/node:" + CalicoVersion,
	"docker.io/calico/pod2daemon-flexvol:" + CalicoVersion,
	"docker.io/calico/typha:" + CalicoVersion,
	"ghcr.io/k8snetworkplumbingwg/multus-cni:snapshot-thick",
	"ghcr.io/k8snetworkplumbingwg/sriov-network-device-plugin:latest",
	"registry.k8s.io/sig-storage/csi-node-driver-registrar:v2.17.0",
	"registry.k8s.io/sig-storage/livenessprobe:v2.19.0",
	"registry.k8s.io/sig-storage/nfsplugin:" + NFSCSIDriverVersion,
}

var AirgapBNKDPUImages = []string{
	"repo.f5.com/images/tmm-img:v10.159.3-0.1.5",
	"repo.f5.com/images/f5dr-img:v3.28.2",
	"repo.f5.com/images/f5dr-img-init:v3.28.2",
	"repo.f5.com/images/tmrouted-img:v2.20.1-0.0.4",
	"repo.f5.com/images/f5-debug-sidecar:v10.63.4-0.1.5",
	"repo.f5.com/images/f5-blobd:v1.24.4-0.0.3",
	"repo.f5.com/images/f5-coremond:v0.16.2",
	"repo.f5.com/images/f5-fluentbit:v1.5.2",
	"repo.f5.com/images/f5-toda-observer:v5.30.13-0.0.5",
	"repo.f5.com/images/f5-eowyn-install:v0.8.4",
	"repo.f5.com/images/f5-node-labeler:v0.0.27",
}

var AirgapKubesprayFiles = []string{
	"https://dl.k8s.io/release/v" + K8sVersionPinned + "/bin/linux/amd64/kubelet",
	"https://dl.k8s.io/release/v" + K8sVersionPinned + "/bin/linux/amd64/kubectl",
	"https://dl.k8s.io/release/v" + K8sVersionPinned + "/bin/linux/amd64/kubeadm",
	"https://github.com/etcd-io/etcd/releases/download/v3.5.22/etcd-v3.5.22-linux-amd64.tar.gz",
	"https://github.com/containernetworking/plugins/releases/download/v1.4.1/cni-plugins-linux-amd64-v1.4.1.tgz",
	"https://github.com/kubernetes-sigs/cri-tools/releases/download/v1.30.1/crictl-v1.30.1-linux-amd64.tar.gz",
	"https://github.com/opencontainers/runc/releases/download/v" + RuncVersion + "/runc.amd64",
	"https://github.com/containerd/containerd/releases/download/v" + ContainerdVer + "/containerd-" + ContainerdVer + "-linux-amd64.tar.gz",
	"https://github.com/containerd/nerdctl/releases/download/v2.0.5/nerdctl-2.0.5-linux-amd64.tar.gz",
}

var AirgapDPUDebs = []string{
	"https://pkgs.k8s.io/core:/stable:/v" + K8sVersion + "/deb/arm64/kubelet_" + K8sVersionPinned + "-1.1_arm64.deb",
	"https://pkgs.k8s.io/core:/stable:/v" + K8sVersion + "/deb/arm64/kubeadm_" + K8sVersionPinned + "-1.1_arm64.deb",
	"https://pkgs.k8s.io/core:/stable:/v" + K8sVersion + "/deb/arm64/kubectl_" + K8sVersionPinned + "-1.1_arm64.deb",
	"https://pkgs.k8s.io/core:/stable:/v" + K8sVersion + "/deb/arm64/cri-tools_1.30.1-1.1_arm64.deb",
	"https://pkgs.k8s.io/core:/stable:/v1.31/deb/arm64/kubernetes-cni_1.5.1-1.1_arm64.deb",
	"https://download.docker.com/linux/ubuntu/dists/noble/pool/stable/arm64/containerd.io_2.2.6-1~ubuntu.24.04~noble_arm64.deb",
}
