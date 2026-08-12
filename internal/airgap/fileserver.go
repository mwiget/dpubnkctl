package airgap

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func StartFileServer(ctx context.Context, w io.Writer, filesDir string) error {
	_ = exec.CommandContext(ctx, "docker", "rm", "-f", FileServerContainer).Run()

	fmt.Fprintf(w, "starting file server on port %d ...\n", FileServerPort)
	cmd := exec.CommandContext(ctx, "docker", "run", "-d",
		"--name", FileServerContainer,
		"--restart=always",
		"-p", fmt.Sprintf("%d:80", FileServerPort),
		"-v", filesDir+":/usr/share/nginx/html:ro",
		FileServerImage,
	)
	cmd.Stdout = w
	cmd.Stderr = w
	if err := cmd.Run(); err != nil {
		return err
	}

	// Wait for the file server to be healthy before returning.
	url := fmt.Sprintf("http://localhost:%d/", FileServerPort)
	for attempt := 1; attempt <= 10; attempt++ {
		time.Sleep(time.Second)
		resp, err := http.Head(url)
		if err == nil {
			resp.Body.Close()
			fmt.Fprintln(w, "file server ready.")
			return nil
		}
		if attempt == 10 {
			return fmt.Errorf("file server not healthy after 10s: %w", err)
		}
	}
	return nil
}

func StopFileServer(ctx context.Context, w io.Writer) error {
	fmt.Fprintln(w, "stopping file server container ...")
	cmd := exec.CommandContext(ctx, "docker", "rm", "-f", FileServerContainer)
	cmd.Stdout = w
	cmd.Stderr = w
	return cmd.Run()
}

type FileEntry struct {
	URL       string // original download URL
	LocalPath string // path under the file server root (mirrors URL structure)
}

func PopulateFileServer(staging string, files []FileEntry) error {
	fsRoot := filepath.Join(staging, FSSubDir)
	for _, f := range files {
		destDir := filepath.Join(fsRoot, filepath.Dir(f.LocalPath))
		if err := os.MkdirAll(destDir, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", destDir, err)
		}
		src := filepath.Join(staging, FilesSubDir, filepath.Base(f.URL))
		dst := filepath.Join(fsRoot, f.LocalPath)
		if err := copyFile(src, dst); err != nil {
			return fmt.Errorf("copy %s -> %s: %w", src, dst, err)
		}
	}
	return nil
}

func DownloadFile(ctx context.Context, w io.Writer, url, destDir string) error {
	cmd := exec.CommandContext(ctx, "curl", "-fSL", "-o",
		filepath.Join(destDir, filepath.Base(url)), url)
	cmd.Stdout = w
	cmd.Stderr = w
	return cmd.Run()
}

func copyFile(src, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		return nil
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o644)
}

func URLToLocalPath(url string) string {
	for _, prefix := range []string{"https://", "http://"} {
		url = strings.TrimPrefix(url, prefix)
	}
	return url
}
