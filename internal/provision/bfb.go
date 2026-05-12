package provision

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/mwiget/dpubnkctl/internal/version"
)

// BFBImagePath returns the absolute local cache path for the configured
// BFB image. Expands ~ in cacheDir.
func BFBImagePath(cacheDir, imageName string) (string, error) {
	if cacheDir == "" {
		cacheDir = "~/.cache/dpubnkctl/bfb"
	}
	if strings.HasPrefix(cacheDir, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		cacheDir = filepath.Join(home, cacheDir[2:])
	}
	return filepath.Join(cacheDir, imageName), nil
}

// EnsureBFB returns the absolute local path to the BFB image, downloading
// it from `url` (or the binary-pinned default) if absent. Reports
// progress via cb (called every ~10% during download).
func EnsureBFB(ctx context.Context, cacheDir, imageName, urlOverride string, progress func(written, total int64)) (string, error) {
	dst, err := BFBImagePath(cacheDir, imageName)
	if err != nil {
		return "", err
	}
	if st, err := os.Stat(dst); err == nil && st.Size() > 0 {
		return dst, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}

	url := urlOverride
	if url == "" {
		url = strings.TrimRight(version.BFBBaseURL, "/") + "/" + imageName
	}

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}

	tmp := dst + ".part"
	out, err := os.Create(tmp)
	if err != nil {
		return "", err
	}

	total := resp.ContentLength
	pr := &progressReader{
		r:           resp.Body,
		total:       total,
		progress:    progress,
		reportEvery: 64 * 1024 * 1024,
	}
	if _, err := io.Copy(out, pr); err != nil {
		out.Close()
		os.Remove(tmp)
		return "", fmt.Errorf("download: %w", err)
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return "", err
	}
	if progress != nil {
		progress(pr.written, total)
	}
	return dst, nil
}

type progressReader struct {
	r           io.Reader
	written     int64
	total       int64
	nextReport  int64
	reportEvery int64
	progress    func(written, total int64)
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.written += int64(n)
		if p.progress != nil && p.written >= p.nextReport {
			p.progress(p.written, p.total)
			p.nextReport += p.reportEvery
		}
	}
	return n, err
}
