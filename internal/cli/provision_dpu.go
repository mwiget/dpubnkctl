package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/mwiget/dpubnkctl/internal/poc"
	"github.com/mwiget/dpubnkctl/internal/provision"
	"github.com/mwiget/dpubnkctl/internal/ssh"
)

type provisionDPUFlags struct {
	pocDir              string
	dpuPCI              string
	yolo                bool
	confirmFlash        string // single name OR comma-separated list matching args
	bfbURL              string
	bfbFetch            string // "" (use PoC) | push | host — overrides provisioning.bfb_fetch
	skipBFBChecksum     bool
	flashTimeout        time.Duration
	dpuWaitTimeout      time.Duration
	bf3SettleTimeout    time.Duration
	skipDPUWait         bool
	skipPostFlashReboot bool
	skipValidate        bool
	skipBF3Settle       bool
}

func newProvisionDPUCmd() *cobra.Command {
	f := &provisionDPUFlags{}
	cmd := &cobra.Command{
		Use:     "dpu <hostname> [<hostname>...]",
		Aliases: []string{"dpus"}, // README + persona docs historically say `dpus`; both work
		Short:   "Flash one or more DPUs end-to-end (DESTRUCTIVE — wipes the DPU OS)",
		Long: `For each <hostname>:
  1. Render bf.conf (Phase 2a)
  2. SSH-connect to the host
  3. Run readiness checks
  4. Cache the BFB image locally (downloaded once, shared across hosts)
  5. SFTP-push BFB + bf.conf to host /tmp
  6. Run bfb-install with streaming output
  7. Wait for DPU SSH, then trigger an rshim soft-reset to clear the
     first-boot LAG race, then wait for DPU SSH again

Multiple hostnames run in parallel — same BFB cache, independent SSH
connections, prefixed output per host. The full per-host bfb-install log
is always written to artifacts/<host>-flash.log unprefixed.

Required gates:
  --yolo                          acknowledge that this is destructive
  --confirm-flash NAME[,NAME...]  must list every hostname argument

The destructive step does not run unless both gates are present and
every host's plan is READY.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runProvisionDPUMulti(cmd.Context(), cmd.OutOrStdout(), args, f)
		},
	}
	cmd.Flags().StringVar(&f.pocDir, "poc", "", "PoC repo path (default: current directory)")
	cmd.Flags().StringVar(&f.dpuPCI, "dpu", "", "DPU PCI address (default: first DPU on each host)")
	cmd.Flags().BoolVar(&f.yolo, "yolo", false, "Acknowledge that this command is destructive")
	cmd.Flags().StringVar(&f.confirmFlash, "confirm-flash", "", "Comma-separated list of hostnames — must match the positional args (typo guard)")
	cmd.Flags().StringVar(&f.bfbURL, "bfb-url", "", "Override the BFB download URL (defaults to binary-pinned)")
	cmd.Flags().StringVar(&f.bfbFetch, "bfb-fetch", "", "How the BFB reaches the host: push (download locally + SFTP) | host (host curls it directly). Overrides provisioning.bfb_fetch")
	cmd.Flags().BoolVar(&f.skipBFBChecksum, "skip-bfb-checksum", false, "Skip the host-side sha256 verification of the BFB (bfb_on_host / bfb_fetch: host) — for the unpinned/dev case")
	cmd.Flags().DurationVar(&f.flashTimeout, "flash-timeout", 25*time.Minute, "Per-flash bfb-install timeout")
	cmd.Flags().DurationVar(&f.dpuWaitTimeout, "dpu-wait-timeout", 10*time.Minute, "How long to wait for the DPU to come back via tmfifo (each wait)")
	cmd.Flags().BoolVar(&f.skipDPUWait, "skip-dpu-wait", false, "Don't wait for the DPU to reboot — return as soon as bfb-install exits (also skips post-flash reboot)")
	cmd.Flags().BoolVar(&f.skipPostFlashReboot, "skip-post-flash-reboot", false, "Skip the rshim SW_RESET after the first DPU boot (workaround retained for diagnostics)")
	cmd.Flags().BoolVar(&f.skipValidate, "skip-validate", false, "Skip the `dpubnkctl validate` precheck (not recommended)")
	cmd.Flags().DurationVar(&f.bf3SettleTimeout, "bf3-settle-timeout", 90*time.Second, "After SF-ready, wait up to this for the host-side mlx5_core PF to come back (clears the ghost-PF state without a reboot)")
	cmd.Flags().BoolVar(&f.skipBF3Settle, "skip-bf3-settle", false, "Don't poll the host-side parent_iface after flash (AGENTS.md #29 ghost-PF recovery then falls back on the operator)")
	return cmd
}

// flashJob bundles everything one parallel goroutine needs.
type flashJob struct {
	hostname string
	host     *poc.Host
	dpu      *poc.DPU
	rendered string
}

// bfb source modes for bfbPlan.mode.
const (
	bfbModePush   = "push"   // download to local cache, SFTP-push to host
	bfbModeOnHost = "onhost" // reuse an operator-pre-staged file (bfb_on_host)
	bfbModeFetch  = "fetch"  // host curls the BFB itself (bfb_fetch: host)
)

// bfbPlan is the resolved decision of how the BFB reaches each host,
// computed once (from PoC + flags) and shared read-only across the
// per-host flash goroutines. Exactly one of localPath (push) / hostPath
// (onhost, fetch) names the image; fetchURL is set only for fetch.
type bfbPlan struct {
	mode         string
	localPath    string // push: local cache path to SFTP to the host
	hostPath     string // onhost/fetch: absolute path of the BFB on the host
	fetchURL     string // fetch: URL the host curls the BFB from
	expectedSHA  string // resolved digest (ExpectedBFBSHA256); "" = unpinned
	skipChecksum bool   // --skip-bfb-checksum
}

func runProvisionDPUMulti(ctx context.Context, out io.Writer, hostnames []string, f *provisionDPUFlags) error {
	repo, err := resolvePoCDir(f.pocDir)
	if err != nil {
		return err
	}
	p, err := poc.Load(repo)
	if err != nil {
		return fmt.Errorf("not a PoC repo (%s): %w", repo, err)
	}

	// rshim: allocate tmfifo addresses before rendering bf.conf (the DPU
	// tmfifo IP is baked into the netplan) and before flashing. Persists
	// poc.yaml so the allocation is recorded for idempotent redeploy.
	if err := ensureTmfifoAllocated(repo, p, out); err != nil {
		return err
	}

	// Dedup + preserve order.
	hostnames = uniqueOrdered(hostnames)

	if !f.yolo {
		return errors.New("refusing destructive flash without --yolo")
	}
	if err := validateConfirmFlash(hostnames, f.confirmFlash); err != nil {
		return err
	}

	// poc.yaml precheck: every PoC that's gotten here without `dpubnkctl
	// validate` clean has stalled at some downstream phase. Surface the
	// errors up front rather than after a BFB cache-and-push that won't
	// be usable. Override with --skip-validate.
	if !f.skipValidate {
		// Only enforce rules whose phase ≤ provision. FAR/JWT/selfip checks
		// fire at deploy and are irrelevant here — refusing to provision a
		// DPU because a JWT isn't on disk yet wastes operator time.
		vr := poc.ValidateForPhase(p, repo, poc.PhaseProvision)
		printValidation(out, vr)
		if !vr.Valid() {
			return fmt.Errorf("poc.yaml has %d provision-phase validation error(s) — fix them or pass --skip-validate", len(vr.Errors))
		}
	}

	// Resolve every job up front; fail fast on missing hosts/DPUs.
	jobs := make([]flashJob, 0, len(hostnames))
	for _, hn := range hostnames {
		host, err := findHost(p, hn)
		if err != nil {
			return err
		}
		dpu, err := pickDPU(host, f.dpuPCI)
		if err != nil {
			return err
		}
		jobs = append(jobs, flashJob{hostname: hn, host: host, dpu: dpu})
	}

	// Pre-render every bf.conf before touching anything destructive.
	// Also persist each render to artifacts/<host>-bf.conf so it's
	// inspectable after the fact (debugging stuck DPUs without having
	// to re-run `provision plan`).
	artifactsDir := filepath.Join(repo, "artifacts")
	if err := os.MkdirAll(artifactsDir, 0o755); err != nil {
		return fmt.Errorf("create artifacts dir: %w", err)
	}
	for i := range jobs {
		rendered, err := provision.Render(p, jobs[i].host, jobs[i].dpu, repo)
		if err != nil {
			return fmt.Errorf("%s: bf.conf render: %w", jobs[i].hostname, err)
		}
		jobs[i].rendered = rendered
		cfgPath := filepath.Join(artifactsDir, jobs[i].hostname+"-bf.conf")
		if err := os.WriteFile(cfgPath, []byte(rendered), 0o644); err != nil {
			return fmt.Errorf("%s: save bf.conf to %s: %w", jobs[i].hostname, cfgPath, err)
		}
	}

	fmt.Fprintf(out, "PoC:    %s   (BNK %s, DOCA %s)\n", p.Metadata.Name, p.Metadata.BNKVersion, p.Versions.DOCA)
	fmt.Fprintf(out, "Image:  %s   (DOCA %s)\n", p.Versions.BFBImage, p.Versions.DOCA)
	fmt.Fprintf(out, "Hosts:  %s\n\n", strings.Join(hostnames, ", "))

	// BFB sourcing. Three modes, resolved once here into a bfbPlan that
	// the per-host goroutines act on:
	//   - push (default): download into the operator-laptop cache
	//     (EnsureBFB, verified against the pin), then SFTP-push to each
	//     host inside pushAndFlash.
	//   - bfb_on_host set: reuse an operator-pre-staged file at the given
	//     absolute path on every host; skip local cache + SFTP push, and
	//     sha256-verify it on the host before flashing.
	//   - bfb_fetch: host: the host curls the BFB itself (never round-
	//     trips the runner), then sha256-verifies it. Both host modes are
	//     for slow/expensive operator uplinks (transatlantic VPN, metered
	//     home internet) where 1.5 GB over the WAN is the bottleneck.
	plan, err := resolveBFBPlan(ctx, out, p, f)
	if err != nil {
		return err
	}

	// Fan out — one goroutine per host. journalMu guards shared journal file.
	var (
		journalMu sync.Mutex
		wg        sync.WaitGroup
	)
	type result struct {
		hostname string
		err      error
	}
	results := make(chan result, len(jobs))

	for _, j := range jobs {
		j := j
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := perHostWriter(out, j.hostname, len(jobs) > 1)
			err := flashOneJob(ctx, w, repo, p, j, plan, f, &journalMu)
			results <- result{hostname: j.hostname, err: err}
		}()
	}
	wg.Wait()
	close(results)

	// Aggregate.
	var failures []string
	for r := range results {
		if r.err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", r.hostname, r.err))
		}
	}

	// Save poc.yaml once. Only mark provision completed if every host succeeded.
	if len(failures) == 0 {
		p.Status.Provision = "completed"
	} else {
		p.Status.Provision = "in_progress"
	}
	p.Status.LastPhaseAt = time.Now().UTC()
	if err := savePoC(repo, p, out); err != nil {
		return err
	}

	if len(failures) > 0 {
		fmt.Fprintln(out)
		for _, fline := range failures {
			fmt.Fprintf(out, "FAIL: %s\n", fline)
		}
		return fmt.Errorf("%d/%d host(s) failed", len(failures), len(jobs))
	}

	fmt.Fprintln(out, "\nDONE.")
	return nil
}

// resolveBFBPlan decides how the BFB reaches each host from the PoC +
// flags, and performs the one-time shared work (the local download for
// push mode). The returned plan is read-only and safe to share across the
// per-host goroutines. Precedence: --bfb-fetch flag > provisioning.bfb_fetch
// > push. An explicit bfb_on_host and bfb_fetch: host are mutually
// exclusive (fail fast). The expected digest resolves PoC > version pin.
func resolveBFBPlan(ctx context.Context, out io.Writer, p *poc.PoC, f *provisionDPUFlags) (bfbPlan, error) {
	plan := bfbPlan{
		expectedSHA:  provision.ExpectedBFBSHA256(p.Provisioning.BFBSHA256),
		skipChecksum: f.skipBFBChecksum,
	}

	fetchMode := p.Provisioning.BFBFetch
	if f.bfbFetch != "" {
		fetchMode = f.bfbFetch
	}
	if fetchMode == "" {
		fetchMode = poc.BFBFetchPush
	}
	if fetchMode != poc.BFBFetchPush && fetchMode != poc.BFBFetchHost {
		return bfbPlan{}, fmt.Errorf("bfb fetch mode %q invalid (must be %s or %s)", fetchMode, poc.BFBFetchPush, poc.BFBFetchHost)
	}

	if plan.expectedSHA == "" && !plan.skipChecksum {
		fmt.Fprintln(out, "[cache] WARN: no BFB sha256 pinned (version pin empty, provisioning.bfb_sha256 unset) — integrity not enforced")
	}

	switch {
	case p.Provisioning.BFBOnHost != "":
		if fetchMode == poc.BFBFetchHost {
			return bfbPlan{}, fmt.Errorf("provisioning.bfb_on_host and bfb_fetch: host are mutually exclusive — unset one (bfb_on_host reuses a manually staged file; bfb_fetch: host curls it for you)")
		}
		plan.mode = bfbModeOnHost
		plan.hostPath = p.Provisioning.BFBOnHost
		fmt.Fprintf(out, "[cache] BFB pre-staged on host at %s — skipping local download + SFTP push\n", plan.hostPath)
		fmt.Fprintf(out, "[cache]   pushAndFlash will stat + sha256-verify the remote path before bfb-install runs\n\n")

	case fetchMode == poc.BFBFetchHost:
		plan.mode = bfbModeFetch
		plan.hostPath = provision.BFBHostPath(p.Provisioning.BFBHostCacheDir, p.Versions.BFBImage)
		plan.fetchURL = provision.BFBDownloadURL(p.Versions.BFBURL, p.Versions.BFBImage)
		fmt.Fprintf(out, "[cache] bfb_fetch: host — each host will curl %s\n", plan.fetchURL)
		fmt.Fprintf(out, "[cache]   into %s, then sha256-verify it (no runner→host push)\n\n", plan.hostPath)

	default: // push
		plan.mode = bfbModePush
		fmt.Fprintf(out, "[cache] BFB (%s) ...\n", p.Versions.BFBImage)
		bfbProgress := func(written, total int64) {
			if total > 0 {
				fmt.Fprintf(out, "[cache]   downloading: %d / %d MiB (%.1f%%)\n",
					written>>20, total>>20, float64(written)/float64(total)*100)
			}
		}
		bfbPath, err := provision.EnsureBFB(ctx, p.Provisioning.BFBCacheDir, p.Versions.BFBImage, p.Versions.BFBURL, plan.expectedSHA, bfbProgress)
		if err != nil {
			return bfbPlan{}, fmt.Errorf("bfb cache: %w", err)
		}
		plan.localPath = bfbPath
		if st, _ := os.Stat(bfbPath); st != nil {
			fmt.Fprintf(out, "[cache]   ready: %s (%d MiB)\n\n", bfbPath, st.Size()>>20)
		}
	}

	return plan, nil
}

// flashOneJob runs the per-host pipeline. Writes are prefixed if w is a
// per-host writer (multi-host mode); otherwise it streams plainly. The
// pipeline is split into five phase helpers so each is ~30 lines and
// individually readable; flashOneJob is now just the orchestrator.
//
// plan carries the resolved BFB source (push local path, or an on-host
// path that's pre-staged or host-fetched) plus the expected digest.
func flashOneJob(ctx context.Context, w io.Writer, repo string, p *poc.PoC, j flashJob, plan bfbPlan, f *provisionDPUFlags, journalMu *sync.Mutex) error {
	logPath := filepath.Join(repo, "artifacts", j.hostname+"-flash.log")

	fmt.Fprintf(w, "DPU:    %s   mode=%s lag=%v hostname=%s tmfifo=%s\n",
		j.dpu.PCI, orDash(j.dpu.Mode), j.dpu.LAG, orDash(j.dpu.Hostname), orDash(j.dpu.TmfifoIP))

	client, err := connectAndCheck(ctx, w, repo, j, journalMu, logPath)
	if err != nil {
		return err
	}
	defer client.Close()

	if err := pushAndFlash(ctx, w, repo, p, j, plan, logPath, client, f.flashTimeout, journalMu); err != nil {
		return err
	}

	waitFirstBoot(ctx, w, client, j, f)

	if err := swResetAndWaitSecondBoot(ctx, w, client, j, f, journalMu, repo, logPath); err != nil {
		return err
	}

	if err := postFlashVerify(ctx, w, repo, j, f, journalMu, logPath); err != nil {
		return err
	}

	journaled(journalMu, repo, j.hostname, j.dpu, "SUCCESS", logPath, "")
	return nil
}

// connectAndCheck dials the host (jumphost-aware) and runs the
// provision readiness probe. Steps 1+2 of the flash pipeline. Returns
// the open SSH client; caller MUST Close.
func connectAndCheck(ctx context.Context, w io.Writer, repo string, j flashJob, journalMu *sync.Mutex, logPath string) (*ssh.Client, error) {
	fmt.Fprintln(w, "[1/7] Connecting to host ...")
	cfg, err := sshConfigForHost(repo, j.host, 30*time.Second)
	if err != nil {
		return nil, err
	}
	dialCtx, dialCancel := context.WithTimeout(ctx, 30*time.Second)
	client, err := ssh.Dial(dialCtx, cfg)
	dialCancel()
	if err != nil {
		return nil, fmt.Errorf("ssh dial: %w", err)
	}
	fmt.Fprintln(w, "      ok")

	fmt.Fprintln(w, "[2/7] Running readiness checks ...")
	probeCtx, pcancel := context.WithTimeout(ctx, 60*time.Second)
	rep := provision.Check(probeCtx, j.host.Name, provision.AsRunner(client), j.dpu.PCI)
	pcancel()
	if !rep.Ready() {
		for _, e := range rep.Errors {
			fmt.Fprintln(w, "      err:", e)
		}
		journaled(journalMu, repo, j.hostname, j.dpu, "FAILED (readiness)", logPath, strings.Join(rep.Errors, "; "))
		_ = client.Close()
		return nil, errors.New("readiness check failed")
	}
	for _, warning := range rep.Warnings {
		fmt.Fprintln(w, "      warn:", warning)
	}
	fmt.Fprintln(w, "      ok")
	return client, nil
}

// pushAndFlash makes the BFB available on the host, pushes the rendered
// bf.conf, and runs bfb-install, streaming output to w + per-host log
// file. Steps 3+4 of the flash pipeline. Journals fine-grained failure
// status (push bfb / push bf.conf / transport / exit N) so post-mortem
// can distinguish them.
//
// Step 3 branches on plan.mode:
//   - push:   SFTP the local cache copy to /tmp (already digest-verified
//     locally by EnsureBFB).
//   - onhost: reuse the operator-pre-staged file; stat it (fail-fast if
//     missing/empty) and sha256-verify it on the host.
//   - fetch:  the host curls the BFB itself (reusing a matching staged
//     copy), then sha256-verifies it.
//
// bf.conf is always pushed (it's rendered per-DPU and small).
func pushAndFlash(ctx context.Context, w io.Writer, repo string, p *poc.PoC, j flashJob, plan bfbPlan, logPath string, client *ssh.Client, flashTimeout time.Duration, journalMu *sync.Mutex) error {
	remoteCfg := "/tmp/dpubnkctl-bf.cfg"
	pushCtx, pushCancel := context.WithTimeout(ctx, 15*time.Minute)
	defer pushCancel()

	remoteBFB, err := prepareRemoteBFB(ctx, pushCtx, w, repo, p, j, plan, logPath, client, journalMu)
	if err != nil {
		return err
	}

	if err := client.PushBytes(pushCtx, []byte(j.rendered), remoteCfg); err != nil {
		journaled(journalMu, repo, j.hostname, j.dpu, "FAILED (push bf.conf)", logPath, err.Error())
		return fmt.Errorf("push bf.conf: %w", err)
	}
	fmt.Fprintf(w, "      pushed %s\n", remoteCfg)

	fmt.Fprintln(w, "[4/7] Running bfb-install (5–15 minutes) ...")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return err
	}
	logFile, err := os.Create(logPath)
	if err != nil {
		return err
	}
	defer logFile.Close()
	tee := io.MultiWriter(prefixWriter{w: w, prefix: "      | "}, logFile)
	flashCmd := fmt.Sprintf("sudo -n bfb-install --bfb '%s' --config '%s' --rshim rshim0 2>&1", remoteBFB, remoteCfg)
	flashCtx, flashCancel := context.WithTimeout(ctx, flashTimeout)
	exit, runErr := client.RunStream(flashCtx, flashCmd, tee)
	flashCancel()
	if runErr != nil {
		journaled(journalMu, repo, j.hostname, j.dpu, "FAILED (transport)", logPath, runErr.Error())
		return fmt.Errorf("bfb-install: %w", runErr)
	}
	if exit != 0 {
		journaled(journalMu, repo, j.hostname, j.dpu, fmt.Sprintf("FAILED (exit %d)", exit), logPath, "")
		return fmt.Errorf("bfb-install exited %d — see %s", exit, logPath)
	}
	fmt.Fprintf(w, "      bfb-install completed — log at %s\n", logPath)
	return nil
}

// prepareRemoteBFB runs step 3's BFB-specific work per plan.mode and
// returns the absolute host path bfb-install should flash from.
func prepareRemoteBFB(ctx, pushCtx context.Context, w io.Writer, repo string, p *poc.PoC, j flashJob, plan bfbPlan, logPath string, client *ssh.Client, journalMu *sync.Mutex) (string, error) {
	switch plan.mode {
	case bfbModeOnHost:
		fmt.Fprintln(w, "[3/7] Verifying pre-staged BFB on host ...")
		statCtx, statCancel := context.WithTimeout(ctx, 30*time.Second)
		size, err := client.RemoteStat(statCtx, plan.hostPath)
		statCancel()
		if err != nil {
			journaled(journalMu, repo, j.hostname, j.dpu, "FAILED (bfb_on_host missing)", logPath, err.Error())
			return "", fmt.Errorf("bfb_on_host %s: %w (stage the BFB on the host before provision dpu, or unset provisioning.bfb_on_host)", plan.hostPath, err)
		}
		if size == 0 {
			journaled(journalMu, repo, j.hostname, j.dpu, "FAILED (bfb_on_host empty)", logPath, plan.hostPath)
			return "", fmt.Errorf("bfb_on_host %s exists but is empty — partial download?", plan.hostPath)
		}
		fmt.Fprintf(w, "      ok: %s (%d MiB)\n", plan.hostPath, size>>20)
		if err := verifyRemoteBFBChecksum(ctx, w, client, plan); err != nil {
			journaled(journalMu, repo, j.hostname, j.dpu, "FAILED (bfb_on_host checksum)", logPath, err.Error())
			return "", err
		}
		return plan.hostPath, nil

	case bfbModeFetch:
		if err := fetchBFBToHost(ctx, w, client, plan); err != nil {
			journaled(journalMu, repo, j.hostname, j.dpu, "FAILED (bfb host fetch)", logPath, err.Error())
			return "", err
		}
		return plan.hostPath, nil

	default: // push
		fmt.Fprintln(w, "[3/7] Pushing BFB + bf.conf to host /tmp ...")
		remoteBFB := "/tmp/" + p.Versions.BFBImage
		pushProgress := func(written, total int64) {
			if total > 0 {
				fmt.Fprintf(w, "      sftp: %d / %d MiB (%.1f%%)\n",
					written>>20, total>>20, float64(written)/float64(total)*100)
			}
		}
		if err := client.PushFile(pushCtx, plan.localPath, remoteBFB, pushProgress); err != nil {
			journaled(journalMu, repo, j.hostname, j.dpu, "FAILED (push bfb)", logPath, err.Error())
			return "", fmt.Errorf("push bfb: %w", err)
		}
		fmt.Fprintf(w, "      pushed %s\n", remoteBFB)
		return remoteBFB, nil
	}
}

// verifyRemoteBFBChecksum runs sha256sum on the host and compares to the
// resolved digest. No-op (logging why) when unpinned or --skip-bfb-checksum.
// A generous timeout covers hashing a 1.5 GB file on slow host storage —
// unlike the WAN push, this is host-local and finishes in seconds to tens
// of seconds. On success prints `host BFB sha256 OK (<digest[:12]>)`.
func verifyRemoteBFBChecksum(ctx context.Context, w io.Writer, client *ssh.Client, plan bfbPlan) error {
	if plan.skipChecksum {
		fmt.Fprintln(w, "      sha256 check skipped (--skip-bfb-checksum)")
		return nil
	}
	if plan.expectedSHA == "" {
		fmt.Fprintln(w, "      sha256 not pinned — skipping host integrity check (integrity not enforced)")
		return nil
	}
	hashCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	got, err := provision.RemoteSHA256(hashCtx, provision.AsRunner(client), plan.hostPath)
	if err != nil {
		return fmt.Errorf("host BFB sha256 failed for %s: %w", plan.hostPath, err)
	}
	if !provision.EqualDigest(got, plan.expectedSHA) {
		return fmt.Errorf("bfb_on_host integrity check failed: got %s, expected %s", got, plan.expectedSHA)
	}
	fmt.Fprintf(w, "      host BFB sha256 OK (%s)\n", got[:12])
	return nil
}

// fetchBFBToHost implements bfb_fetch: host. It reuses a staged copy whose
// sha256 already matches (or, unpinned, any non-empty file), otherwise it
// has the host curl the BFB atomically and then verifies it. Streams curl
// progress to w.
func fetchBFBToHost(ctx context.Context, w io.Writer, client *ssh.Client, plan bfbPlan) error {
	fmt.Fprintf(w, "[3/7] Fetching BFB on host (%s) ...\n", plan.hostPath)

	// Reuse path: a matching staged copy means no re-download.
	if plan.expectedSHA != "" && !plan.skipChecksum {
		hashCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		got, herr := provision.RemoteSHA256(hashCtx, provision.AsRunner(client), plan.hostPath)
		cancel()
		if herr == nil && provision.EqualDigest(got, plan.expectedSHA) {
			fmt.Fprintf(w, "      reusing staged BFB — host sha256 OK (%s)\n", got[:12])
			return nil
		}
		if herr == nil {
			fmt.Fprintf(w, "      staged BFB sha256 mismatch (got %s) — re-fetching\n", got[:12])
		}
	} else {
		statCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		size, serr := client.RemoteStat(statCtx, plan.hostPath)
		cancel()
		if serr == nil && size > 0 {
			fmt.Fprintf(w, "      reusing staged BFB %s (%d MiB; integrity not verified)\n", plan.hostPath, size>>20)
			return nil
		}
	}

	fmt.Fprintf(w, "      curl %s ...\n", plan.fetchURL)
	fetchCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	exit, err := client.RunStream(fetchCtx, provision.HostFetchCommand(plan.fetchURL, plan.hostPath), prefixWriter{w: w, prefix: "      | "})
	if err != nil {
		return fmt.Errorf("host BFB fetch: %w", err)
	}
	if exit != 0 {
		return fmt.Errorf("host BFB fetch exited %d — ensure curl is installed on the host and it can reach %s (see bfb_url / BFBBaseURL)", exit, plan.fetchURL)
	}
	fmt.Fprintf(w, "      fetched %s\n", plan.hostPath)
	return verifyRemoteBFBChecksum(ctx, w, client, plan)
}

// waitFirstBoot polls for DPU SSH reachability after bfb-install's
// first boot. Non-fatal — swResetAndWaitSecondBoot is the real gate,
// so a first-boot timeout here is logged as a warning and we still
// proceed to SW_RESET. Step 5 of the flash pipeline.
func waitFirstBoot(ctx context.Context, w io.Writer, client *ssh.Client, j flashJob, f *provisionDPUFlags) {
	addrs := dpuProbeAddrs(j.dpu)
	dpuName := dpuLabel(j.dpu)
	if f.skipDPUWait || len(addrs) == 0 {
		fmt.Fprintln(w, "[5/7] (skipping DPU SSH wait)")
		return
	}
	fmt.Fprintf(w, "[5/7] Waiting for %s first boot (probing %s) ...\n", dpuName, strings.Join(addrs, ", "))
	if err := waitForDPUSSHWithGrace(ctx, client, j.host, addrs, f.dpuWaitTimeout); err != nil {
		fmt.Fprintf(w, "      WARN: first boot wait timed out (%v) — continuing to SW_RESET anyway\n", err)
		return
	}
	fmt.Fprintf(w, "      %s is reachable.\n", dpuName)
}

// swResetAndWaitSecondBoot issues an rshim SW_RESET on the host and
// waits for the DPU to come back. The second-boot reachability gate
// is hard — if the DPU doesn't return, this returns an error and the
// flash is marked POST_FLASH_UNREACHABLE so downstream phases don't
// press on into cluster-join time. Steps 6+7 of the flash pipeline.
func swResetAndWaitSecondBoot(ctx context.Context, w io.Writer, client *ssh.Client, j flashJob, f *provisionDPUFlags, journalMu *sync.Mutex, repo, logPath string) error {
	addrs := dpuProbeAddrs(j.dpu)
	dpuName := dpuLabel(j.dpu)
	skip := f.skipPostFlashReboot || f.skipDPUWait
	if skip {
		fmt.Fprintln(w, "[6/7] (skipping post-flash reboot)")
		fmt.Fprintln(w, "[7/7] (no second boot wait)")
		return nil
	}
	fmt.Fprintln(w, "[6/7] Triggering rshim SW_RESET (clears first-boot LAG race) ...")
	if r := client.Run(ctx, "echo 'SW_RESET 1' | sudo -n tee /dev/rshim0/misc > /dev/null"); !r.OK() {
		fmt.Fprintf(w, "      WARN: SW_RESET failed (%s) — DPU still up but FW may need manual reset\n", strings.TrimSpace(r.Stderr+r.Stdout))
	} else {
		fmt.Fprintln(w, "      reset issued; allowing DPU to reboot ...")
	}
	if len(addrs) == 0 {
		fmt.Fprintln(w, "[7/7] (no second boot wait — tmfifo_ip and oob_ip both empty)")
		return nil
	}
	fmt.Fprintf(w, "[7/7] Waiting for %s second boot (probing %s) ...\n", dpuName, strings.Join(addrs, ", "))
	// Give the reset a moment to actually start before polling.
	time.Sleep(8 * time.Second)
	if err := waitForDPUSSHWithGrace(ctx, client, j.host, addrs, f.dpuWaitTimeout); err != nil {
		journaled(journalMu, repo, j.hostname, j.dpu, "POST_FLASH_UNREACHABLE", logPath, err.Error())
		return fmt.Errorf("DPU %s never came back after SW_RESET (timeout + grace expired) — probed %s; check rshim console for kernel panic, BL2 hang, or PCIe reset failure", dpuName, strings.Join(addrs, ", "))
	}
	fmt.Fprintf(w, "      %s is reachable after reboot.\n", dpuName)
	return nil
}

// postFlashVerify enforces the two post-flash invariants:
//
//  1. SR-IOV sub-functions (mlx5_core.sf.*) exist for both PFs. Hard
//     fail — without them TMM stays Pending on `Insufficient
//     nvidia.com/bf3_p0_sf1`. Skipped if we never reached the DPU.
//
//  2. Host-side parent_iface mlx5_core PF is out of ghost state. Soft
//     fail — host network setup has its own modprobe-reload + PCIe-
//     rescan recovery, so we just WARN and move on.
//
// Skipped entirely when the operator opted out of post-flash waits.
func postFlashVerify(ctx context.Context, w io.Writer, repo string, j flashJob, f *provisionDPUFlags, journalMu *sync.Mutex, logPath string) error {
	// Verification needs a way to reach the DPU (verifyDPUSubFunctions
	// runs `mlnx-sf -a show` over SSH). Skip when no probe address is
	// configured at all — matches the skip logic in swResetAndWaitSecondBoot.
	skip := f.skipPostFlashReboot || f.skipDPUWait || len(dpuProbeAddrs(j.dpu)) == 0
	if !skip {
		if err := verifyDPUSubFunctions(ctx, repo, j, w); err != nil {
			journaled(journalMu, repo, j.hostname, j.dpu, "POST_FLASH_SF_INCOMPLETE", logPath, err.Error())
			return err
		}
	}
	if !f.skipBF3Settle && j.host.DataPlane != nil && j.host.DataPlane.ParentIface != "" {
		if err := waitForHostParentIface(ctx, repo, j, f.bf3SettleTimeout, w); err != nil {
			fmt.Fprintf(w, "      WARN: parent_iface settle wait did not converge: %v\n", err)
			fmt.Fprintln(w, "      `host network setup` will attempt mlx5_core reload to recover.")
		}
	}
	return nil
}

// dpuLabel returns the DPU hostname, or "DPU" if unset. Operator-
// facing progress lines use this so the tmfifo IP (which is the same
// across every DPU) doesn't appear and confuse anyone scanning
// parallel multi-host output.
func dpuLabel(d *poc.DPU) string {
	if d != nil && d.Hostname != "" {
		return d.Hostname
	}
	return "DPU"
}

// waitForHostParentIface polls `ip link show <parent_iface>` on the
// host until the kernel returns a real netdev (not the ghost state
// where the sysfs entry exists but the kernel reports "No such
// device"). On a healthy post-flash this converges in 10-30s. Returns
// nil on success, error on timeout — the caller decides whether to
// hard-fail or punt to `host network setup`'s mlx5_core recovery.
func waitForHostParentIface(ctx context.Context, repo string, j flashJob, timeout time.Duration, w io.Writer) error {
	cfg, err := sshConfigForHost(repo, j.host, 30*time.Second)
	if err != nil {
		return err
	}
	dialCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	c, err := ssh.Dial(dialCtx, cfg)
	cancel()
	if err != nil {
		return fmt.Errorf("ssh to host for settle wait: %w", err)
	}
	defer c.Close()

	parent := j.host.DataPlane.ParentIface
	fmt.Fprintf(w, "      Waiting for host-side %s to be live (BF3 settle, up to %s) ...\n",
		parent, timeout)

	pollCtx, pollCancel := context.WithTimeout(ctx, timeout)
	defer pollCancel()
	start := time.Now()
	for {
		// `ethtool -i` returns "No such device" on the ghost state but
		// non-zero exit AND empty driver line; a healthy PF prints
		// `driver: mlx5_core` in stdout.
		r := c.Run(pollCtx, fmt.Sprintf("ethtool -i %s 2>&1 | grep -E '^driver:' || true", parent))
		if r.OK() && strings.Contains(r.Stdout, "mlx5_core") {
			fmt.Fprintf(w, "      host %s is live after %s.\n", parent, time.Since(start).Round(time.Second))
			return nil
		}
		if pollCtx.Err() != nil {
			return fmt.Errorf("host %s still in ghost state after %s", parent, timeout)
		}
		time.Sleep(3 * time.Second)
	}
}

// verifyDPUSubFunctions SSHes into the freshly-flashed DPU and counts
// mlx5_core sub-function aux devices. PER_PF_NUM_SF=1 (set by the bf.conf
// mlxconfig step) means we should see exactly 2 — one each for p0/p1.
// Polls for up to 60s because the mlnx-sf systemd unit may still be
// settling when sshd accepted the connection.
//
// On failure, returns an error whose text includes the recovery recipe
// the operator can run on the DPU directly. AGENTS.md #7 covers the
// underlying race in narrative form.
func verifyDPUSubFunctions(ctx context.Context, repo string, j flashJob, w io.Writer) error {
	cfg, err := dpuSSHConfig(repo, j.host, j.dpu)
	if err != nil {
		fmt.Fprintf(w, "      WARN: skipping SF readiness check (%v)\n", err)
		return nil
	}
	fmt.Fprintf(w, "      Verifying %s SR-IOV sub-functions are present ...\n", j.dpu.Hostname)

	const want = 2
	pollCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	dial, dcancel := context.WithTimeout(pollCtx, 30*time.Second)
	c, err := ssh.Dial(dial, cfg)
	dcancel()
	if err != nil {
		fmt.Fprintf(w, "      WARN: skipping SF readiness check (ssh to DPU: %v)\n", err)
		return nil
	}
	defer c.Close()

	var lastCount int
	for {
		// `ls -1 /sys/bus/auxiliary/devices/` enumerates every aux
		// device; mlx5_core.sf.<n> is the SF entry. wc -l gives count.
		r := c.Run(pollCtx, `ls /sys/bus/auxiliary/devices/ 2>/dev/null | grep -cE '^mlx5_core\.sf\.[0-9]+$' || true`)
		if r.OK() {
			n := 0
			fmt.Sscanf(strings.TrimSpace(r.Stdout), "%d", &n)
			lastCount = n
			if n >= want {
				fmt.Fprintf(w, "      %d/%d SR-IOV sub-functions present.\n", n, want)
				return nil
			}
		}
		select {
		case <-pollCtx.Done():
			return fmt.Errorf(
				"DPU %s has %d/%d SR-IOV sub-functions after second boot — "+
					"TMM would stay Pending on `Insufficient nvidia.com/bf3_p0_sf1`. "+
					"Recovery: ssh to the DPU and run `sudo /sbin/mlnx-sf --action create "+
					"--device <pci-of-missing-pf> --sfnum 1 --enable-trust --hwaddr <random-mac>` "+
					"(see /etc/mellanox/mlnx-sf.conf for the original create commands). "+
					"AGENTS.md #7 documents the underlying race",
				j.dpu.Hostname, lastCount, want)
		case <-time.After(3 * time.Second):
		}
	}
}


// perHostWriter returns a writer that prefixes lines with [hostname] when
// running multi-host (so parallel streams are tellable apart). For
// single-host runs it returns out unwrapped to keep the original UX.
func perHostWriter(out io.Writer, hostname string, multi bool) io.Writer {
	if !multi {
		return out
	}
	return &lockedPrefixWriter{w: out, prefix: "[" + hostname + "] "}
}

// lockedPrefixWriter is a prefixWriter that serializes whole-line writes
// across goroutines. Without the lock, concurrent prefix+payload writes
// could interleave inside a single line.
type lockedPrefixWriter struct {
	mu     sync.Mutex
	w      io.Writer
	prefix string
	pend   bytes.Buffer // partial-line buffer
}

func (l *lockedPrefixWriter) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	written := 0
	for len(b) > 0 {
		nl := bytes.IndexByte(b, '\n')
		if nl < 0 {
			l.pend.Write(b)
			return written + len(b), nil
		}
		// Emit pending buffer + this line as one prefixed write.
		var line []byte
		if l.pend.Len() > 0 {
			line = append(line, l.pend.Bytes()...)
			l.pend.Reset()
		}
		line = append(line, b[:nl+1]...)
		full := append([]byte(l.prefix), line...)
		if _, err := l.w.Write(full); err != nil {
			return written, err
		}
		written += nl + 1
		b = b[nl+1:]
	}
	return written, nil
}

// validateConfirmFlash requires --confirm-flash to list exactly the
// hostname args (set equality), guarding against typos that would flash
// the wrong machine.
func validateConfirmFlash(args []string, raw string) error {
	if raw == "" {
		return errors.New("--confirm-flash is required (must list every hostname argument, comma-separated)")
	}
	got := map[string]bool{}
	for _, s := range strings.Split(raw, ",") {
		got[strings.TrimSpace(s)] = true
	}
	want := map[string]bool{}
	for _, h := range args {
		want[h] = true
	}
	if len(got) != len(want) {
		return fmt.Errorf("--confirm-flash %q does not match the %d hostname argument(s) %v", raw, len(args), args)
	}
	for h := range want {
		if !got[h] {
			return fmt.Errorf("--confirm-flash missing hostname %q (typo guard)", h)
		}
	}
	for h := range got {
		if !want[h] {
			return fmt.Errorf("--confirm-flash lists %q which is not in the hostname args", h)
		}
	}
	return nil
}

func uniqueOrdered(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func journaled(mu *sync.Mutex, repo, hostname string, dpu *poc.DPU, status, logPath, errMsg string) {
	mu.Lock()
	defer mu.Unlock()
	appendFlashJournal(repo, hostname, dpu, status, logPath, errMsg)
}

// prefixWriter prefixes every line of streamed bfb-install output. Used
// inside flashOneJob's tee to disambiguate tool output from wrapper
// messages. Single-goroutine — no locking needed (bfb-install is the
// only writer per host, and stdout is goroutine-local via perHostWriter).
type prefixWriter struct {
	w      io.Writer
	prefix string
}

func (p prefixWriter) Write(b []byte) (int, error) {
	written := 0
	for len(b) > 0 {
		nl := bytes.IndexByte(b, '\n')
		if nl < 0 {
			if _, err := p.w.Write(append([]byte(p.prefix), b...)); err != nil {
				return written, err
			}
			return written + len(b), nil
		}
		line := b[:nl+1]
		if _, err := p.w.Write(append([]byte(p.prefix), line...)); err != nil {
			return written, err
		}
		written += len(line)
		b = b[nl+1:]
	}
	return written, nil
}

func sshConfigForHost(repo string, h *poc.Host, timeout time.Duration) (ssh.Config, error) {
	keyPath := h.SSH.KeyRef
	if !filepath.IsAbs(keyPath) {
		keyPath = filepath.Join(repo, keyPath)
	}
	if _, err := os.Stat(keyPath); err != nil {
		return ssh.Config{}, fmt.Errorf("ssh key %s: %w", keyPath, err)
	}
	cfg := ssh.Config{
		Address: h.SSH.Address, Port: h.SSH.Port,
		User: h.SSH.User, KeyPath: keyPath,
		KnownHosts: filepath.Join(repo, "inventory", "known_hosts"),
		Timeout:    timeout,
	}
	if h.SSH.Jumphost != "" {
		// Default: reuse the target's user + key for the jumphost hop.
		// Override either independently via SSH.JumphostUser /
		// SSH.JumphostKeyRef when the jumphost account is authorised
		// with a different identity (common when the operator's
		// workstation key opens the jumphost while a separate per-lab
		// key opens the targets behind it).
		jumpUser := h.SSH.User
		if h.SSH.JumphostUser != "" {
			jumpUser = h.SSH.JumphostUser
		}
		jumpKey := keyPath
		if h.SSH.JumphostKeyRef != "" {
			jumpKey = h.SSH.JumphostKeyRef
			if !filepath.IsAbs(jumpKey) {
				jumpKey = filepath.Join(repo, jumpKey)
			}
			if _, err := os.Stat(jumpKey); err != nil {
				return ssh.Config{}, fmt.Errorf("ssh jumphost key %s: %w", jumpKey, err)
			}
		}
		cfg.Jumphost = &ssh.Config{
			Address: h.SSH.Jumphost, Port: 22,
			User: jumpUser, KeyPath: jumpKey,
			KnownHosts: filepath.Join(repo, "inventory", "known_hosts"),
			Timeout:    timeout,
		}
	}
	return cfg, nil
}

func tmfifoHostPart(cidr string) string {
	if cidr == "" {
		return ""
	}
	if i := strings.IndexByte(cidr, '/'); i > 0 {
		return cidr[:i]
	}
	return cidr
}

// dpuProbeAddrs returns the list of IPv4 addresses on which the DPU
// SHOULD answer SSH after bfb-install: tmfifo first (the canonical
// rshim path the operator's host can reach without the lab fabric),
// then oob_ip if set (independent of rshim's host-side .1/30 state).
//
// Two-target probing is what makes the ailab single-node PoC robust:
// a `systemctl restart rshim` (e.g. operator clearing an orphaned
// console reader) wipes the host's 192.168.100.1/30 on tmfifo_net0,
// after which the tmfifo route to 192.168.100.2 silently fails. With
// oob_ip in the probe list, the wait succeeds anyway because the DPU
// is reachable over its dedicated mgmt path.
func dpuProbeAddrs(d *poc.DPU) []string {
	if d == nil {
		return nil
	}
	var addrs []string
	if ip := tmfifoHostPart(d.TmfifoIP); ip != "" {
		addrs = append(addrs, ip)
	}
	if ip := tmfifoHostPart(d.OOBIP); ip != "" {
		addrs = append(addrs, ip)
	}
	return addrs
}

// ensureHostTmfifoIP idempotently brings tmfifo_net0 up and assigns
// 192.168.100.1/30 on the host. The rshim kernel module *should* do
// this at module load, but `systemctl restart rshim` wipes the address;
// without it the second-boot SSH wait silently times out because the
// 192.168.100.2 route doesn't exist on the host. Errors are swallowed —
// the most common is "RTNETLINK answers: File exists" when the address
// is already present, which is the success case.
func ensureHostTmfifoIP(ctx context.Context, host *ssh.Client) {
	ensureHostTmfifoIPCIDR(ctx, host, poc.DefaultTmfifoHostIP)
}

// ensureHostTmfifoIPFor is ensureHostTmfifoIP for a specific host, using
// its allocated host-side tmfifo CIDR (host.tmfifo_ip). For the pool case
// (network.tmfifo_cidr) the host lives on a carved /30, not the rshim
// default — using the wrong address means the host can't route to the
// DPU's tmfifo IP.
func ensureHostTmfifoIPFor(ctx context.Context, host *ssh.Client, h *poc.Host) {
	ensureHostTmfifoIPCIDR(ctx, host, h.TmfifoHostIP())
}

// ensureHostTmfifoIPCIDR assigns the given CIDR to tmfifo_net0. When the
// CIDR differs from the rshim driver default, it first deletes the
// default 192.168.100.1/30 so the two don't coexist and shadow the pool
// address — this is the root fix for the historic "dup tmfifo" scramble
// when more than one host reused 192.168.100.x. All commands are
// best-effort/idempotent ("File exists" on a repeat add is the success
// case), so errors are swallowed.
func ensureHostTmfifoIPCIDR(ctx context.Context, host *ssh.Client, cidr string) {
	if cidr == "" {
		cidr = poc.DefaultTmfifoHostIP
	}
	cmd := "sudo -n ip link set tmfifo_net0 up 2>/dev/null; "
	if cidr != poc.DefaultTmfifoHostIP {
		cmd += "sudo -n ip addr del " + poc.DefaultTmfifoHostIP + " dev tmfifo_net0 2>/dev/null; "
	}
	cmd += "sudo -n ip addr add " + cidr + " dev tmfifo_net0 2>/dev/null; true"
	_ = host.Run(ctx, cmd)
}

// waitForDPUSSHWithGrace runs the primary wait, and on timeout extends
// once for an additional 30s "grace" before reporting failure. The
// homelab PoC saw both boot waits expire on the millisecond while the
// DPU was actually responding — rshim/cloud-init settling races the
// primary timeout. One extra burst absorbs that without raising the
// primary (which would slow the happy path).
//
// addrs is the list of candidate IPv4 addresses to probe (tmfifo, then
// oob). Returns nil if ANY address responds.
func waitForDPUSSHWithGrace(ctx context.Context, host *ssh.Client, h *poc.Host, addrs []string, primary time.Duration) error {
	if len(addrs) == 0 {
		return fmt.Errorf("no DPU probe addresses configured (tmfifo_ip and oob_ip both empty)")
	}
	ensureHostTmfifoIPFor(ctx, host, h)
	first, fcancel := context.WithTimeout(ctx, primary)
	err := waitForDPUSSH(first, host, addrs)
	fcancel()
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return err
	}
	grace, gcancel := context.WithTimeout(ctx, 30*time.Second)
	defer gcancel()
	return waitForDPUSSH(grace, host, addrs)
}

// waitForDPUSSH polls TCP/22 from the host on each address in addrs
// every 5 s. Returns nil as soon as ANY address responds with "open" —
// so a tmfifo-route gap (host-side .1/30 wiped by an rshim restart, or
// kernel rshim module not yet ready) doesn't mask a DPU that's
// perfectly reachable via its oob_ip.
func waitForDPUSSH(ctx context.Context, host *ssh.Client, addrs []string) error {
	if len(addrs) == 0 {
		return fmt.Errorf("no DPU probe addresses")
	}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		for _, ip := range addrs {
			cmd := fmt.Sprintf("timeout 4 bash -c '</dev/tcp/%s/22' && echo open || echo closed", ip)
			r := host.Run(ctx, cmd)
			if r.OK() && strings.TrimSpace(r.Stdout) == "open" {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

func appendFlashJournal(repo, hostname string, dpu *poc.DPU, status, logPath, errMsg string) {
	header := fmt.Sprintf("flash DPU %s on %s — %s", dpu.PCI, hostname, status)
	f, err := openJournal(repo, "provision", header)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "- DPU: pci=%s mode=%s lag=%v hostname=%s tmfifo=%s\n",
		dpu.PCI, orDash(dpu.Mode), dpu.LAG, orDash(dpu.Hostname), orDash(dpu.TmfifoIP))
	fmt.Fprintf(f, "- bfb-install log: %s\n", strings.TrimPrefix(logPath, repo+string(filepath.Separator)))
	if errMsg != "" {
		fmt.Fprintf(f, "- Error: %s\n", errMsg)
	}
	fmt.Fprintln(f, "- Next: pre-sales SE confirms DPU reachability + decides cluster phase")
	fmt.Fprintln(f)
}
