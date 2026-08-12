package airgap

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/mwiget/dpubnkctl/internal/deploy"
	"github.com/mwiget/dpubnkctl/internal/version"
)

func DownloadAndPushImages(ctx context.Context, w io.Writer, cfg *Config) error {
	imgDir := filepath.Join(cfg.StagingDir, ImagesSubDir)
	if err := os.MkdirAll(imgDir, 0o755); err != nil {
		return err
	}

	all := append(version.AirgapKubesprayImages, version.AirgapNetworkImages...)
	all = append(all, version.AirgapCertManagerImages...)
	all = append(all, version.AirgapNFSImages...)
	for i, img := range all {
		name := tarName(img)
		tarPath := filepath.Join(imgDir, name)
		short := StripRegistryPrefix(img)
		regRef := fmt.Sprintf("%s/%s", cfg.RegistryHost, short)

		fmt.Fprintf(w, "[%d/%d] %s ...\n", i+1, len(all), img)
		if err := PullAndPush(ctx, w, img, tarPath, regRef, "amd64"); err != nil {
			return fmt.Errorf("image %s: %w", img, err)
		}
	}
	return nil
}

func DownloadAndPushBNKHostImages(ctx context.Context, w io.Writer, cfg *Config) error {
	imgDir := filepath.Join(cfg.StagingDir, ImagesSubDir)
	if err := os.MkdirAll(imgDir, 0o755); err != nil {
		return err
	}

	for i, img := range version.AirgapBNKHostImages {
		name := tarName(img)
		tarPath := filepath.Join(imgDir, name)
		short := StripRegistryPrefix(img)
		regRef := fmt.Sprintf("%s/%s", cfg.RegistryHost, short)

		fmt.Fprintf(w, "[%d/%d] %s (amd64) ...\n", i+1, len(version.AirgapBNKHostImages), img)
		if err := PullAndPush(ctx, w, img, tarPath, regRef, "amd64"); err != nil {
			return fmt.Errorf("bnk host image %s: %w", img, err)
		}
	}
	return nil
}

func DownloadDPUKubesprayImages(ctx context.Context, w io.Writer, cfg *Config) error {
	dpuDir := filepath.Join(cfg.StagingDir, DPUImgSubDir)
	if err := os.MkdirAll(dpuDir, 0o755); err != nil {
		return err
	}

	for i, img := range version.AirgapDPUKubesprayImages {
		name := tarName(img)
		tarPath := filepath.Join(dpuDir, name)

		fmt.Fprintf(w, "[%d/%d] %s (arm64) ...\n", i+1, len(version.AirgapDPUKubesprayImages), img)
		if err := PullToTar(ctx, w, img, tarPath, "arm64"); err != nil {
			return fmt.Errorf("dpu kubespray image %s: %w", img, err)
		}
	}
	return nil
}

func DownloadBNKDPUImages(ctx context.Context, w io.Writer, cfg *Config) error {
	dpuDir := filepath.Join(cfg.StagingDir, DPUImgSubDir)
	if err := os.MkdirAll(dpuDir, 0o755); err != nil {
		return err
	}

	for i, img := range version.AirgapBNKDPUImages {
		name := tarName(img)
		tarPath := filepath.Join(dpuDir, name)

		fmt.Fprintf(w, "[%d/%d] %s (arm64) ...\n", i+1, len(version.AirgapBNKDPUImages), img)
		if err := PullToTar(ctx, w, img, tarPath, "arm64"); err != nil {
			return fmt.Errorf("bnk dpu image %s: %w", img, err)
		}
	}
	return nil
}

func DownloadBinaries(ctx context.Context, w io.Writer, cfg *Config) error {
	filesDir := filepath.Join(cfg.StagingDir, FilesSubDir)
	if err := os.MkdirAll(filesDir, 0o755); err != nil {
		return err
	}

	for i, url := range version.AirgapKubesprayFiles {
		fmt.Fprintf(w, "[%d/%d] %s ...\n", i+1, len(version.AirgapKubesprayFiles), filepath.Base(url))
		if err := DownloadFile(ctx, w, url, filesDir); err != nil {
			return fmt.Errorf("download %s: %w", url, err)
		}
	}
	return nil
}

func DownloadDPUDebs(ctx context.Context, w io.Writer, cfg *Config) error {
	debDir := filepath.Join(cfg.StagingDir, DPUDebSubDir)
	if err := os.MkdirAll(debDir, 0o755); err != nil {
		return err
	}

	for i, url := range version.AirgapDPUDebs {
		fmt.Fprintf(w, "[%d/%d] %s ...\n", i+1, len(version.AirgapDPUDebs), filepath.Base(url))
		if err := DownloadFile(ctx, w, url, debDir); err != nil {
			return fmt.Errorf("download %s: %w", url, err)
		}
	}
	return nil
}

func DownloadReleaseManifest(ctx context.Context, w io.Writer, cfg *Config, farAuth deploy.OCIAuth) error {
	cacheDir := filepath.Join(strings.TrimSuffix(cfg.StagingDir, StagingDir), "artifacts", "release-manifest")
	fmt.Fprintf(w, "  pulling release-manifest %s ...\n", version.CNEManifestVersion)
	_, err := deploy.PullReleaseManifest(ctx, farAuth, version.CNEManifestVersion, cacheDir)
	return err
}

func DownloadFLOChart(ctx context.Context, w io.Writer, cfg *Config, farAuth deploy.OCIAuth) error {
	chartsDir := filepath.Join(cfg.StagingDir, ChartsSubDir)
	if err := os.MkdirAll(chartsDir, 0o755); err != nil {
		return err
	}
	absChartsDir, _ := filepath.Abs(chartsDir)
	cacheDir := filepath.Join(strings.TrimSuffix(cfg.StagingDir, StagingDir), "artifacts", "release-manifest")
	m, err := deploy.LoadCachedManifest(cacheDir)
	if err != nil {
		return fmt.Errorf("load manifest for FLO version: %w", err)
	}
	floVer := m.Chart("charts/f5-lifecycle-operator")
	if floVer == "" {
		return fmt.Errorf("FLO chart version not found in release manifest")
	}
	fmt.Fprintf(w, "  pulling FLO chart %s ...\n", floVer)
	script := fmt.Sprintf(`cat | helm registry login %s --username "$USERNAME" --password-stdin >/dev/null && helm pull %s --version %s -d /work`,
		version.FARRegistryHost, version.FLOChartOCIRef, floVer)
	cmd := execCmd(ctx, "docker", "run", "--rm", "-i",
		"-v", absChartsDir+":/work",
		"--network=host",
		"-e", "USERNAME="+farAuth.Username,
		version.K8sToolsImage, "sh", "-c", script)
	cmd.Stdin = strings.NewReader(farAuth.Password + "\n")
	cmd.Stdout = w
	cmd.Stderr = w
	return cmd.Run()
}

func DownloadCertManagerManifest(ctx context.Context, w io.Writer, cfg *Config) error {
	chartsDir := filepath.Join(cfg.StagingDir, ChartsSubDir)
	if err := os.MkdirAll(chartsDir, 0o755); err != nil {
		return err
	}
	cmURL := fmt.Sprintf("https://github.com/cert-manager/cert-manager/releases/download/%s/cert-manager.yaml", version.CertManagerVersion)
	outPath := filepath.Join(chartsDir, "cert-manager.yaml")
	fmt.Fprintf(w, "  downloading cert-manager %s ...\n", version.CertManagerVersion)
	return DownloadFile(ctx, w, cmURL, filepath.Dir(outPath))
}

func DownloadCertGenChart(ctx context.Context, w io.Writer, cfg *Config, farAuth deploy.OCIAuth) error {
	chartsDir := filepath.Join(cfg.StagingDir, ChartsSubDir)
	if err := os.MkdirAll(chartsDir, 0o755); err != nil {
		return err
	}
	absChartsDir, _ := filepath.Abs(chartsDir)
	cacheDir := filepath.Join(strings.TrimSuffix(cfg.StagingDir, StagingDir), "artifacts", "release-manifest")
	m, err := deploy.LoadCachedManifest(cacheDir)
	if err != nil {
		return fmt.Errorf("load manifest for cert-gen version: %w", err)
	}
	certGenVer := m.Chart("utils/f5-cert-gen")
	if certGenVer == "" {
		return fmt.Errorf("cert-gen chart version not found in release manifest")
	}
	ref := "oci://repo.f5.com/utils/f5-cert-gen"
	fmt.Fprintf(w, "  pulling cert-gen chart %s ...\n", certGenVer)
	script := fmt.Sprintf(`cat | helm registry login %s --username "$USERNAME" --password-stdin >/dev/null && helm pull %s --version %s -d /work`,
		version.FARRegistryHost, ref, certGenVer)
	cmd := execCmd(ctx, "docker", "run", "--rm", "-i",
		"-v", absChartsDir+":/work",
		"--network=host",
		"-e", "USERNAME="+farAuth.Username,
		version.K8sToolsImage, "sh", "-c", script)
	cmd.Stdin = strings.NewReader(farAuth.Password + "\n")
	cmd.Stdout = w
	cmd.Stderr = w
	return cmd.Run()
}

func DownloadTigeraOperatorManifest(ctx context.Context, w io.Writer, cfg *Config) error {
	manifestsDir := filepath.Join(cfg.StagingDir, "manifests")
	if err := os.MkdirAll(manifestsDir, 0o755); err != nil {
		return err
	}
	fmt.Fprintf(w, "  downloading tigera-operator %s ...\n", version.TigeraOperatorVersion)
	return DownloadFile(ctx, w, version.TigeraOperatorManifest, manifestsDir)
}

func DownloadNFSCSIChart(ctx context.Context, w io.Writer, cfg *Config) error {
	chartsDir := filepath.Join(cfg.StagingDir, ChartsSubDir)
	if err := os.MkdirAll(chartsDir, 0o755); err != nil {
		return err
	}
	absChartsDir, _ := filepath.Abs(chartsDir)
	fmt.Fprintf(w, "  pulling NFS CSI chart %s ...\n", version.NFSCSIDriverVersion)
	cmd := execCmd(ctx, "docker", "run", "--rm",
		"-v", absChartsDir+":/work",
		"--network=host",
		version.K8sToolsImage, "helm", "pull",
		version.NFSCSIChartName,
		"--repo", version.NFSCSIChartRepo,
		"--version", version.NFSCSIDriverVersion,
		"-d", "/work")
	cmd.Stdout = w
	cmd.Stderr = w
	return cmd.Run()
}

func execCmd(ctx context.Context, name string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, name, args...)
}

func PopulateAndStartFileServer(ctx context.Context, w io.Writer, cfg *Config) error {
	entries := make([]FileEntry, len(version.AirgapKubesprayFiles))
	for i, url := range version.AirgapKubesprayFiles {
		entries[i] = FileEntry{
			URL:       url,
			LocalPath: URLToLocalPath(url),
		}
	}
	if err := PopulateFileServer(cfg.StagingDir, entries); err != nil {
		return fmt.Errorf("populate file server: %w", err)
	}

	fsDir := filepath.Join(cfg.StagingDir, FSSubDir)
	return StartFileServer(ctx, w, fsDir)
}

func LoadPreStagedImages(ctx context.Context, w io.Writer, cfg *Config) error {
	imgDir := filepath.Join(cfg.StagingDir, ImagesSubDir)
	entries, err := os.ReadDir(imgDir)
	if err != nil {
		return fmt.Errorf("read images dir: %w", err)
	}

	for i, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".tar") {
			continue
		}
		tarPath := filepath.Join(imgDir, e.Name())
		img := tarToImageRef(tarPath)
		if img == "" {
			fmt.Fprintf(w, "  skipping %s (cannot determine image ref)\n", e.Name())
			continue
		}
		short := StripRegistryPrefix(img)
		regRef := fmt.Sprintf("%s/%s", cfg.RegistryHost, short)

		fmt.Fprintf(w, "[%d/%d] loading %s ...\n", i+1, len(entries), e.Name())
		if err := PushImage(ctx, w, tarPath, regRef); err != nil {
			return fmt.Errorf("load %s: %w", e.Name(), err)
		}
	}
	return nil
}

func tarName(imageRef string) string {
	short := StripRegistryPrefix(imageRef)
	safe := strings.ReplaceAll(short, "/", "__")
	return strings.ReplaceAll(safe, ":", "@@") + ".tar"
}

func tarToImageRef(tarPath string) string {
	name := filepath.Base(tarPath)
	name = strings.TrimSuffix(name, ".tar")
	if !strings.Contains(name, "@@") {
		// Legacy naming (colon encoded as hyphen) — fall back to
		// last-hyphen split for backward compatibility.
		idx := strings.LastIndex(name, "-")
		if idx < 0 {
			return ""
		}
		ref := name[:idx] + ":" + name[idx+1:]
		return strings.ReplaceAll(ref, "__", "/")
	}
	ref := strings.Replace(name, "@@", ":", 1)
	return strings.ReplaceAll(ref, "__", "/")
}
