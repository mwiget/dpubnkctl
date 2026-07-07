package provision

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/mwiget/dpubnkctl/internal/discover"
	"github.com/mwiget/dpubnkctl/internal/poc"
)

// BFBHostPath returns the absolute path where `bfb_fetch: host` stages the
// image on the host: <cacheDir>/<imageName>, defaulting cacheDir to
// poc.DefaultBFBHostCacheDir when empty. imageName is validated elsewhere
// (safeBFBNameRe) so this is a plain join — no ~ expansion, the host cache
// dir is a system path, not the operator's home.
func BFBHostPath(cacheDir, imageName string) string {
	if strings.TrimSpace(cacheDir) == "" {
		cacheDir = poc.DefaultBFBHostCacheDir
	}
	return path.Join(cacheDir, imageName)
}

// HostSHA256Command builds the shell command that hashes hostPath on the
// host. Uses `sudo -n` because the host cache dir (/var/cache/...) and a
// root-owned pre-staged BFB may not be readable by the SSH user — the
// same passwordless-sudo assumption `bfb-install` already relies on.
func HostSHA256Command(hostPath string) string {
	return "sudo -n sha256sum " + shellQuote(hostPath)
}

// HostFetchCommand builds the shell command that fetches url to hostPath
// on the host atomically: create the cache dir, curl to a .part sidecar
// with retries, then rename into place so a killed transfer never leaves a
// truncated file that looks complete. `sudo -n` throughout because the
// cache dir is typically root-owned. curl's -f fails the command on HTTP
// errors (so a 404 from a bad bfb_url surfaces as a non-zero exit rather
// than a saved error page).
func HostFetchCommand(url, hostPath string) string {
	dir := path.Dir(hostPath)
	part := hostPath + ".part"
	return fmt.Sprintf(
		"sudo -n mkdir -p %s && sudo -n curl -fSL --retry 5 -o %s %s && sudo -n mv -f %s %s",
		shellQuote(dir), shellQuote(part), shellQuote(url), shellQuote(part), shellQuote(hostPath),
	)
}

// RemoteSHA256 runs sha256sum on the host over runner and returns the
// parsed hex digest. Errors if the command fails (e.g. the file is
// missing) or the output doesn't parse — callers use the error to
// distinguish "no file yet" from a real digest.
func RemoteSHA256(ctx context.Context, runner discover.Runner, hostPath string) (string, error) {
	r := runner.Run(ctx, HostSHA256Command(hostPath))
	if !r.OK() {
		detail := strings.TrimSpace(r.Stderr + r.Stdout)
		if r.Err != nil {
			return "", fmt.Errorf("sha256sum %s: %w", hostPath, r.Err)
		}
		return "", fmt.Errorf("sha256sum %s exited %d: %s", hostPath, r.ExitCode, detail)
	}
	return ParseSHA256SumOutput(r.Stdout)
}

// shellQuote wraps s in single quotes for safe interpolation into a host
// command line. Any embedded single quote is closed, escaped, and
// reopened (the standard POSIX-sh idiom), so shell metacharacters in
// paths/URLs cannot break out regardless of upstream validation.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
