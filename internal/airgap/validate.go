package airgap

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mwiget/dpubnkctl/internal/version"
)

func ValidateInfra(ctx context.Context, w io.Writer, cfg *Config) error {
	fmt.Fprintln(w, "validating airgap infrastructure ...")

	if err := validateRegistry(ctx, w, cfg.RegistryHost); err != nil {
		return fmt.Errorf("registry check: %w", err)
	}

	if err := validateFileServer(ctx, w, cfg.FilesRepo); err != nil {
		return fmt.Errorf("file server check: %w", err)
	}

	if err := validateFileServerURLs(ctx, w, cfg.FilesRepo); err != nil {
		return fmt.Errorf("file server content check: %w", err)
	}

	if err := validateStagingDir(w, cfg.StagingDir, DPUImgSubDir, ".tar", "DPU image tars"); err != nil {
		return err
	}

	if err := validateStagingDir(w, cfg.StagingDir, DPUDebSubDir, ".deb", "DPU debs"); err != nil {
		return err
	}

	chartsDir := filepath.Join(cfg.StagingDir, ChartsSubDir)
	if _, err := os.Stat(chartsDir); err == nil {
		n := countFiles(chartsDir, "")
		fmt.Fprintf(w, "  charts: %d files\n", n)
	}

	fmt.Fprintln(w, "airgap infrastructure OK")
	return nil
}

func validateRegistry(ctx context.Context, w io.Writer, registryHost string) error {
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: tlsSkipVerify(),
		},
	}
	url := fmt.Sprintf("https://%s/v2/_catalog", registryHost)
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("cannot reach registry at %s: %w", registryHost, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("registry returned status %d", resp.StatusCode)
	}

	var catalog struct {
		Repositories []string `json:"repositories"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&catalog); err != nil {
		return fmt.Errorf("parse catalog: %w", err)
	}

	fmt.Fprintf(w, "  registry: %d repositories\n", len(catalog.Repositories))
	if len(catalog.Repositories) == 0 {
		return fmt.Errorf("registry is empty — no images loaded")
	}
	return nil
}

func validateFileServer(ctx context.Context, w io.Writer, filesRepo string) error {
	client := &http.Client{Timeout: 5 * time.Second}
	testPath := "dl.k8s.io/release/v1.30.14/bin/linux/amd64/kubeadm"
	url := strings.TrimSuffix(filesRepo, "/") + "/" + testPath

	resp, err := client.Head(url)
	if err != nil {
		return fmt.Errorf("cannot reach file server at %s: %w", filesRepo, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("file server returned %d for %s", resp.StatusCode, testPath)
	}

	fmt.Fprintf(w, "  file server: reachable (%s)\n", filesRepo)
	return nil
}

func validateFileServerURLs(ctx context.Context, w io.Writer, filesRepo string) error {
	client := &http.Client{Timeout: 5 * time.Second}
	var failed []string
	for _, url := range version.AirgapKubesprayFiles {
		localPath := URLToLocalPath(url)
		fullURL := strings.TrimSuffix(filesRepo, "/") + "/" + localPath
		resp, err := client.Head(fullURL)
		if err != nil || resp.StatusCode != http.StatusOK {
			failed = append(failed, filepath.Base(url))
			continue
		}
		resp.Body.Close()
	}
	if len(failed) > 0 {
		return fmt.Errorf("%d file(s) not served: %s", len(failed), strings.Join(failed, ", "))
	}
	fmt.Fprintf(w, "  file server: all %d binaries served\n", len(version.AirgapKubesprayFiles))
	return nil
}

func validateStagingDir(w io.Writer, stagingDir, subDir, suffix, label string) error {
	dir := filepath.Join(stagingDir, subDir)
	if _, err := os.Stat(dir); err != nil {
		return fmt.Errorf("%s directory missing: %s", label, dir)
	}
	n := countFiles(dir, suffix)
	if n == 0 {
		return fmt.Errorf("no %s found in %s", label, dir)
	}
	fmt.Fprintf(w, "  %s: %d files\n", label, n)
	return nil
}

func ValidatePreStaged(w io.Writer, stagingDir string) error {
	fmt.Fprintln(w, "validating pre-staged offline packages ...")

	dirs := []string{
		filepath.Join(stagingDir, ImagesSubDir),
		filepath.Join(stagingDir, DPUImgSubDir),
		filepath.Join(stagingDir, FilesSubDir),
		filepath.Join(stagingDir, ChartsSubDir),
		filepath.Join(stagingDir, DPUDebSubDir),
	}
	for _, d := range dirs {
		info, err := os.Stat(d)
		if err != nil || !info.IsDir() {
			return fmt.Errorf("missing directory: %s", d)
		}
	}

	imgCount := countFiles(filepath.Join(stagingDir, ImagesSubDir), ".tar")
	dpuCount := countFiles(filepath.Join(stagingDir, DPUImgSubDir), ".tar")
	fileCount := countFiles(filepath.Join(stagingDir, FilesSubDir), "")

	fmt.Fprintf(w, "  images: %d tarballs\n", imgCount)
	fmt.Fprintf(w, "  dpu-images: %d tarballs\n", dpuCount)
	fmt.Fprintf(w, "  files: %d binaries\n", fileCount)

	if imgCount == 0 {
		return fmt.Errorf("no image tarballs found in %s", filepath.Join(stagingDir, ImagesSubDir))
	}
	if fileCount == 0 {
		return fmt.Errorf("no binary files found in %s", filepath.Join(stagingDir, FilesSubDir))
	}

	fmt.Fprintln(w, "pre-staged packages OK")
	return nil
}

func countFiles(dir, suffix string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && (suffix == "" || strings.HasSuffix(e.Name(), suffix)) {
			n++
		}
	}
	return n
}
