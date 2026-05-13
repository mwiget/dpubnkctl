package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mwiget/dpubnkctl/internal/poc"
	"github.com/mwiget/dpubnkctl/internal/version"
)

// status of a single doctor check.
type checkResult int

const (
	checkOK checkResult = iota
	checkWarn
	checkFail
	checkInfo
)

func (s checkResult) symbol() string {
	switch s {
	case checkOK:
		return "ok  "
	case checkWarn:
		return "warn"
	case checkFail:
		return "fail"
	default:
		return "    "
	}
}

type check struct {
	name   string
	result checkResult
	detail string
}

func newDoctorCmd() *cobra.Command {
	var (
		pocDir       string
		strict       bool
		skipNetwork  bool
		netTimeoutMs int
	)
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Run preflight checks: host tools, network reachability, PoC sanity",
		Long: `Verify the operator workstation is ready to drive a BNK deploy:

  - docker daemon is reachable
  - git is installed (used by ` + "`dpubnkctl init`" + `)
  - container images required at runtime are cached locally (optional)
  - mgmt-network can reach the URLs dpubnkctl pulls from at runtime
  - if a poc.yaml is present, customer-supplied keys are in place

Exits non-zero on any failure. Warnings (e.g. missing optional cached
image) don't fail unless --strict is set.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDoctor(cmd.Context(), cmd.OutOrStdout(), pocDir, strict, skipNetwork, time.Duration(netTimeoutMs)*time.Millisecond)
		},
	}
	cmd.Flags().StringVar(&pocDir, "poc", "", "PoC repo path for poc.yaml sanity checks (default: current directory)")
	cmd.Flags().BoolVar(&strict, "strict", false, "Treat warnings as failures (non-zero exit)")
	cmd.Flags().BoolVar(&skipNetwork, "skip-network", false, "Skip mgmt-network reachability checks (faster, offline-friendly)")
	cmd.Flags().IntVar(&netTimeoutMs, "net-timeout-ms", 3000, "Per-host TCP connect timeout for network checks")
	return cmd
}

func runDoctor(ctx context.Context, out io.Writer, pocDir string, strict, skipNetwork bool, netTimeout time.Duration) error {
	fmt.Fprintf(out, "dpubnkctl %s  (commit %s, BNK %s)\n\n",
		version.Version, version.Commit, version.BNKVersion)

	var checks []check

	// --- host tools ---
	fmt.Fprintln(out, "Host tools")
	checks = append(checks, runAndPrint(out, checkDockerDaemon(ctx)))
	checks = append(checks, runAndPrint(out, checkGit(ctx)))

	// --- cached container images (informational) ---
	fmt.Fprintln(out, "\nContainer images (informational — pulled on demand)")
	checks = append(checks, runAndPrint(out, checkImageCached(ctx, version.KubesprayImage)))
	checks = append(checks, runAndPrint(out, checkImageCached(ctx, version.K8sToolsImage)))

	// --- network reachability ---
	if !skipNetwork {
		fmt.Fprintln(out, "\nMgmt-network reachability")
		for _, target := range []struct{ name, hostport, purpose string }{
			{"content.mellanox.com:443", "content.mellanox.com:443", "BFB image download"},
			{"github.com:443", "github.com:443", "cert-manager release manifests"},
			{"quay.io:443", "quay.io:443", "kubespray container image"},
			{"repo.f5.com:443", "repo.f5.com:443", "FLO Helm chart + BNK container images"},
			{"registry-1.docker.io:443", "registry-1.docker.io:443", "alpine/k8s container image"},
		} {
			checks = append(checks, runAndPrint(out, checkTCP(target.name, target.hostport, target.purpose, netTimeout)))
		}
	} else {
		fmt.Fprintln(out, "\nMgmt-network reachability  (skipped: --skip-network)")
	}

	// --- PoC sanity (only when a poc.yaml exists) ---
	repo, _ := resolvePoCDir(pocDir)
	if p, err := poc.Load(repo); err == nil {
		fmt.Fprintf(out, "\nPoC sanity  (%s)\n", repo)
		for _, c := range pocChecks(repo, p) {
			checks = append(checks, runAndPrint(out, c))
		}
	} else {
		fmt.Fprintln(out, "\nPoC sanity  (no poc.yaml in cwd — pass --poc <dir> to check a specific PoC)")
	}

	// --- summary ---
	var nFail, nWarn, nOK int
	for _, c := range checks {
		switch c.result {
		case checkFail:
			nFail++
		case checkWarn:
			nWarn++
		case checkOK:
			nOK++
		}
	}
	fmt.Fprintf(out, "\n%d ok, %d warning(s), %d failure(s)\n", nOK, nWarn, nFail)
	if nFail > 0 || (strict && nWarn > 0) {
		return fmt.Errorf("doctor: %d failure(s), %d warning(s)", nFail, nWarn)
	}
	return nil
}

func runAndPrint(out io.Writer, c check) check {
	fmt.Fprintf(out, "  [%s] %s", c.result.symbol(), c.name)
	if c.detail != "" {
		fmt.Fprintf(out, " — %s", c.detail)
	}
	fmt.Fprintln(out)
	return c
}

// --- individual checks ---

func checkDockerDaemon(ctx context.Context) check {
	if _, err := exec.LookPath("docker"); err != nil {
		return check{name: "docker", result: checkFail, detail: "not on PATH (install Docker Engine: https://docs.docker.com/engine/install/)"}
	}
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(dctx, "docker", "version", "--format", "{{.Server.Version}}").Output()
	if err != nil {
		return check{name: "docker", result: checkFail, detail: "daemon not responding (`docker version` failed) — start the docker service"}
	}
	return check{name: "docker", result: checkOK, detail: "server " + strings.TrimSpace(string(out))}
}

func checkGit(ctx context.Context) check {
	if _, err := exec.LookPath("git"); err != nil {
		return check{name: "git", result: checkWarn, detail: "not on PATH — `dpubnkctl init` requires git unless invoked with --no-git"}
	}
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(dctx, "git", "--version").Output()
	if err != nil {
		return check{name: "git", result: checkWarn, detail: "`git --version` failed"}
	}
	return check{name: "git", result: checkOK, detail: strings.TrimSpace(string(out))}
}

func checkImageCached(ctx context.Context, image string) check {
	if _, err := exec.LookPath("docker"); err != nil {
		return check{name: "image " + image, result: checkInfo, detail: "docker missing — skipping"}
	}
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := exec.CommandContext(dctx, "docker", "image", "inspect", image).Run()
	if err != nil {
		return check{name: "image " + image, result: checkWarn, detail: "not cached locally — will be pulled on first use"}
	}
	return check{name: "image " + image, result: checkOK, detail: "cached"}
}

func checkTCP(name, hostport, purpose string, timeout time.Duration) check {
	d := net.Dialer{Timeout: timeout}
	c, err := d.Dial("tcp", hostport)
	if err != nil {
		return check{name: name, result: checkFail, detail: fmt.Sprintf("%s (needed for %s)", err, purpose)}
	}
	_ = c.Close()
	return check{name: name, result: checkOK, detail: purpose}
}

func pocChecks(repo string, p *poc.PoC) []check {
	var out []check

	jwt := resolveRef(repo, p.BNK.JWTRef)
	if _, err := os.Stat(jwt); err != nil {
		out = append(out, check{name: "JWT (" + p.BNK.JWTRef + ")", result: checkFail, detail: "missing — drop the F5 TEEM token here"})
	} else {
		out = append(out, check{name: "JWT (" + p.BNK.JWTRef + ")", result: checkOK})
	}

	far := resolveRef(repo, p.BNK.FARKeyRef)
	if _, err := os.Stat(far); err != nil {
		out = append(out, check{name: "FAR (" + p.BNK.FARKeyRef + ")", result: checkFail, detail: "missing — drop the f5-far-auth-key tarball here"})
	} else {
		out = append(out, check{name: "FAR (" + p.BNK.FARKeyRef + ")", result: checkOK})
	}

	if len(p.Hosts) == 0 {
		out = append(out, check{name: "hosts in poc.yaml", result: checkWarn, detail: "no hosts yet — run `dpubnkctl discover wizard`"})
		return out
	}

	for _, h := range p.Hosts {
		if h.SSH.KeyRef == "" {
			out = append(out, check{name: "ssh key for " + h.Name, result: checkWarn, detail: "no key_ref set"})
			continue
		}
		key := h.SSH.KeyRef
		if !filepath.IsAbs(key) {
			key = filepath.Join(repo, key)
		}
		if _, err := os.Stat(key); err != nil {
			out = append(out, check{name: "ssh key for " + h.Name + " (" + h.SSH.KeyRef + ")", result: checkFail, detail: "missing"})
		} else {
			out = append(out, check{name: "ssh key for " + h.Name + " (" + h.SSH.KeyRef + ")", result: checkOK})
		}
	}
	return out
}
