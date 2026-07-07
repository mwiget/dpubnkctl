package provision

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
// progress via cb (called every ~10% during download). expectedSHA is the
// resolved digest to verify against (see ExpectedBFBSHA256); empty skips
// verification.
func EnsureBFB(ctx context.Context, cacheDir, imageName, urlOverride, expectedSHA string, progress func(written, total int64)) (string, error) {
	dst, err := BFBImagePath(cacheDir, imageName)
	if err != nil {
		return "", err
	}
	if st, err := os.Stat(dst); err == nil && st.Size() > 0 {
		if err := verifyBFBChecksum(dst, expectedSHA); err != nil {
			return "", err
		}
		return dst, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}

	url := BFBDownloadURL(urlOverride, imageName)

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
	if err := verifyBFBChecksum(dst, expectedSHA); err != nil {
		// Rename succeeded but the downloaded content doesn't match the
		// pinned hash. Remove the bad file so the next run re-downloads.
		_ = os.Remove(dst)
		return "", err
	}
	if progress != nil {
		progress(pr.written, total)
	}
	return dst, nil
}

// ExpectedBFBSHA256 resolves the digest the BFB should hash to, applying
// the precedence the spec fixes: a per-PoC provisioning.bfb_sha256
// (pocSHA) overrides the binary-pinned version.BFBImageSHA256. An empty
// result means integrity is not pinned anywhere (trust-on-first-use) and
// callers should skip verification (validate warns separately).
func ExpectedBFBSHA256(pocSHA string) string {
	if s := strings.TrimSpace(pocSHA); s != "" {
		return s
	}
	return strings.TrimSpace(version.BFBImageSHA256)
}

// BFBDownloadURL returns the URL to fetch the BFB from: the override
// verbatim if set (it already names the full image URL), otherwise the
// binary-pinned base joined with imageName. Shared by the local download
// (EnsureBFB) and the host-direct fetch (bfb_fetch: host) so both honour
// the same override.
func BFBDownloadURL(urlOverride, imageName string) string {
	if urlOverride != "" {
		return urlOverride
	}
	return strings.TrimRight(version.BFBBaseURL, "/") + "/" + imageName
}

// verifyBFBChecksum hashes the file at path and compares to want. Returns
// nil if want is empty (unpinned) or the hash matches; an error if want is
// set and doesn't match. Callers resolve want via ExpectedBFBSHA256.
func verifyBFBChecksum(path, want string) error {
	want = strings.TrimSpace(want)
	if want == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("sha256 %s: %w", path, err)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !EqualDigest(got, want) {
		return fmt.Errorf("BFB integrity check failed for %s: got sha256=%s, expected %s", path, got, want)
	}
	return nil
}

// EqualDigest compares two hex sha256 digests case-insensitively after
// trimming surrounding whitespace. `sha256sum` emits lowercase, but a
// hand-pasted poc.yaml pin might be upper- or mixed-case.
func EqualDigest(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

// ParseSHA256SumOutput extracts the hex digest from `sha256sum` output,
// which is "<64-hex><space><space><path>" (coreutils) — some builds emit
// a single space or a "*" binary marker, so we take the first field and
// validate it is exactly 64 hex chars. Returns an error on unparseable
// output so a truncated/error line (e.g. "sha256sum: <path>: No such
// file") never silently reads as a mismatch.
func ParseSHA256SumOutput(out string) (string, error) {
	fields := strings.Fields(strings.TrimSpace(out))
	if len(fields) == 0 {
		return "", fmt.Errorf("empty sha256sum output")
	}
	digest := strings.TrimPrefix(fields[0], "*") // some tools mark binary mode
	if !isHex64(digest) {
		return "", fmt.Errorf("unexpected sha256sum output %q (want a 64-char hex digest as the first field)", strings.TrimSpace(out))
	}
	return strings.ToLower(digest), nil
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
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
