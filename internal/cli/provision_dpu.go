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
	cmd.Flags().DurationVar(&f.flashTimeout, "flash-timeout", 25*time.Minute, "Per-flash bfb-install timeout")
	cmd.Flags().DurationVar(&f.dpuWaitTimeout, "dpu-wait-timeout", 10*time.Minute, "How long to wait for the DPU to come back via tmfifo (each wait)")
	cmd.Flags().BoolVar(&f.skipDPUWait, "skip-dpu-wait", false, "Don't wait for the DPU to reboot — return as soon as bfb-install exits (also skips post-flash reboot)")
	cmd.Flags().BoolVar(&f.skipPostFlashReboot, "skip-post-flash-reboot", false, "Skip the rshim SW_RESET after the first DPU boot (workaround retained for diagnostics)")
	cmd.Flags().BoolVar(&f.skipValidate, "skip-validate", false, "Skip the `dpubnkctl validate` precheck (not recommended)")
	cmd.Flags().DurationVar(&f.bf3SettleTimeout, "bf3-settle-timeout", 90*time.Second, "After SF-ready, wait up to this for the host-side mlx5_core PF to come back (clears the ghost-PF state without a reboot)")
	cmd.Flags().BoolVar(&f.skipBF3Settle, "skip-bf3-settle", false, "Don't poll the host-side parent_iface after flash (AGENTS.md #11 host reboot then falls back on the operator)")
	return cmd
}

// flashJob bundles everything one parallel goroutine needs.
type flashJob struct {
	hostname string
	host     *poc.Host
	dpu      *poc.DPU
	rendered string
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

	// Cache BFB once on the operator laptop, shared across all jobs.
	fmt.Fprintf(out, "[cache] BFB (%s) ...\n", p.Versions.BFBImage)
	bfbProgress := func(written, total int64) {
		if total > 0 {
			fmt.Fprintf(out, "[cache]   downloading: %d / %d MiB (%.1f%%)\n",
				written>>20, total>>20, float64(written)/float64(total)*100)
		}
	}
	bfbPath, err := provision.EnsureBFB(ctx, p.Provisioning.BFBCacheDir, p.Versions.BFBImage, p.Versions.BFBURL, bfbProgress)
	if err != nil {
		return fmt.Errorf("bfb cache: %w", err)
	}
	if st, _ := os.Stat(bfbPath); st != nil {
		fmt.Fprintf(out, "[cache]   ready: %s (%d MiB)\n\n", bfbPath, st.Size()>>20)
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
			err := flashOneJob(ctx, w, repo, p, j, bfbPath, f, &journalMu)
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

// flashOneJob runs the per-host pipeline. Writes are prefixed if w is a
// per-host writer (multi-host mode); otherwise it streams plainly.
func flashOneJob(ctx context.Context, w io.Writer, repo string, p *poc.PoC, j flashJob, bfbPath string, f *provisionDPUFlags, journalMu *sync.Mutex) error {
	logPath := filepath.Join(repo, "artifacts", j.hostname+"-flash.log")

	fmt.Fprintf(w, "DPU:    %s   mode=%s lag=%v hostname=%s tmfifo=%s\n",
		j.dpu.PCI, orDash(j.dpu.Mode), j.dpu.LAG, orDash(j.dpu.Hostname), orDash(j.dpu.TmfifoIP))

	// 1. SSH connect.
	fmt.Fprintln(w, "[1/7] Connecting to host ...")
	cfg, err := sshConfigForHost(repo, j.host, 30*time.Second)
	if err != nil {
		return err
	}
	dialCtx, dialCancel := context.WithTimeout(ctx, 30*time.Second)
	client, err := ssh.Dial(dialCtx, cfg)
	dialCancel()
	if err != nil {
		return fmt.Errorf("ssh dial: %w", err)
	}
	defer client.Close()
	fmt.Fprintln(w, "      ok")

	// 2. Readiness.
	fmt.Fprintln(w, "[2/7] Running readiness checks ...")
	probeCtx, pcancel := context.WithTimeout(ctx, 60*time.Second)
	rep := provision.Check(probeCtx, j.host.Name, provision.AsRunner(client), j.dpu.PCI)
	pcancel()
	if !rep.Ready() {
		for _, e := range rep.Errors {
			fmt.Fprintln(w, "      err:", e)
		}
		journaled(journalMu, repo, j.hostname, j.dpu, "FAILED (readiness)", logPath, strings.Join(rep.Errors, "; "))
		return errors.New("readiness check failed")
	}
	for _, warning := range rep.Warnings {
		fmt.Fprintln(w, "      warn:", warning)
	}
	fmt.Fprintln(w, "      ok")

	// 3. SFTP push (BFB + rendered bf.conf).
	fmt.Fprintln(w, "[3/7] Pushing BFB + bf.conf to host /tmp ...")
	remoteBFB := "/tmp/" + p.Versions.BFBImage
	remoteCfg := "/tmp/dpubnkctl-bf.cfg"
	pushCtx, pushCancel := context.WithTimeout(ctx, 15*time.Minute)
	pushProgress := func(written, total int64) {
		if total > 0 {
			fmt.Fprintf(w, "      sftp: %d / %d MiB (%.1f%%)\n",
				written>>20, total>>20, float64(written)/float64(total)*100)
		}
	}
	if err := client.PushFile(pushCtx, bfbPath, remoteBFB, pushProgress); err != nil {
		pushCancel()
		journaled(journalMu, repo, j.hostname, j.dpu, "FAILED (push bfb)", logPath, err.Error())
		return fmt.Errorf("push bfb: %w", err)
	}
	if err := client.PushBytes(pushCtx, []byte(j.rendered), remoteCfg); err != nil {
		pushCancel()
		journaled(journalMu, repo, j.hostname, j.dpu, "FAILED (push bf.conf)", logPath, err.Error())
		return fmt.Errorf("push bf.conf: %w", err)
	}
	pushCancel()
	fmt.Fprintf(w, "      pushed %s + %s\n", remoteBFB, remoteCfg)

	// 4. bfb-install — stream to operator + per-host log file.
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
	flashCtx, flashCancel := context.WithTimeout(ctx, f.flashTimeout)
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

	// 5. Wait for first DPU boot. Identify the DPU by hostname in
	//    progress output — the tmfifo IP is host-local and identical
	//    across every DPU, so reporting it confuses the operator (and
	//    any agent relaying messages). The transport is implicitly
	//    "tmfifo" since that's the only path until oob_net0 comes up.
	dpuIP := tmfifoHostPart(j.dpu.TmfifoIP)
	dpuName := j.dpu.Hostname
	if dpuName == "" {
		dpuName = "DPU"
	}
	if f.skipDPUWait || dpuIP == "" {
		fmt.Fprintln(w, "[5/7] (skipping DPU SSH wait)")
	} else {
		fmt.Fprintf(w, "[5/7] Waiting for %s first boot (via tmfifo) ...\n", dpuName)
		// First-boot wait is heuristic — we SW_RESET afterwards and the
		// real gate is the post-reset wait in step 7. Stay non-fatal here.
		if err := waitForDPUSSHWithGrace(ctx, client, dpuIP, f.dpuWaitTimeout); err != nil {
			fmt.Fprintf(w, "      WARN: first boot wait timed out (%v) — continuing to SW_RESET anyway\n", err)
		} else {
			fmt.Fprintf(w, "      %s is reachable.\n", dpuName)
		}
	}

	// 6. Post-flash rshim soft-reset to clear first-boot LAG/firmware race.
	if !f.skipPostFlashReboot && !f.skipDPUWait {
		fmt.Fprintln(w, "[6/7] Triggering rshim SW_RESET (clears first-boot LAG race) ...")
		r := client.Run(ctx, "echo 'SW_RESET 1' | sudo -n tee /dev/rshim0/misc > /dev/null")
		if !r.OK() {
			fmt.Fprintf(w, "      WARN: SW_RESET failed (%s) — DPU still up but FW may need manual reset\n", strings.TrimSpace(r.Stderr+r.Stdout))
		} else {
			fmt.Fprintln(w, "      reset issued; allowing DPU to reboot ...")
		}
	} else {
		fmt.Fprintln(w, "[6/7] (skipping post-flash reboot)")
	}

	// 7. Wait for second DPU boot (after the soft-reset). This IS the
	//    gate — if the DPU doesn't come back after SW_RESET it's not
	//    going to come back without operator intervention. Treat the
	//    timeout (after grace) as a hard failure so journal records
	//    FAILED and the operator (or e2e) doesn't press on into
	//    cluster-join time only to discover the DPU is dead.
	if !f.skipPostFlashReboot && !f.skipDPUWait && dpuIP != "" {
		fmt.Fprintf(w, "[7/7] Waiting for %s second boot (via tmfifo) ...\n", dpuName)
		// Give the reset a moment to actually start before polling.
		time.Sleep(8 * time.Second)
		if err := waitForDPUSSHWithGrace(ctx, client, dpuIP, f.dpuWaitTimeout); err != nil {
			journaled(journalMu, repo, j.hostname, j.dpu, "POST_FLASH_UNREACHABLE", logPath, err.Error())
			return fmt.Errorf("DPU %s never came back after SW_RESET (timeout + grace expired) — check rshim console for kernel panic, BL2 hang, or PCIe reset failure", dpuName)
		}
		fmt.Fprintf(w, "      %s is reachable after reboot.\n", dpuName)
	} else {
		fmt.Fprintln(w, "[7/7] (no second boot wait)")
	}

	// Final readiness gate: confirm the DPU created both SR-IOV
	// sub-functions (one per PF) that TMM later claims as devices.
	// The BSP's mlnx-sf systemd unit races kernel module init and
	// sometimes only creates one — TMM then stays Pending forever on
	// "Insufficient nvidia.com/bf3_p0_sf1". Fail flash-completion with
	// a recovery recipe so the operator catches it now rather than
	// after `deploy cne`. Skip if we couldn't even reach the DPU
	// (we already warned above) or if the operator opted out of
	// post-flash waits.
	if !f.skipPostFlashReboot && !f.skipDPUWait && dpuIP != "" {
		if err := verifyDPUSubFunctions(ctx, repo, j, w); err != nil {
			journaled(journalMu, repo, j.hostname, j.dpu, "POST_FLASH_SF_INCOMPLETE", logPath, err.Error())
			return err
		}
	}

	// Settle wait: after the DPU OS is up + SF aux devices are present,
	// the host-side mlx5_core driver may still be in the post-flash
	// "ghost PF" state (AGENTS.md #11). For BlueField-3 cards on Proxmox
	// VFIO-passthrough this is especially dangerous: a host reboot at
	// this moment can hang the kernel's PCIe reset path and take the
	// hypervisor down (verified on rome1 with VMs 204/205, May 15).
	//
	// Instead of reboot-or-die, just poll the host's parent_iface until
	// `ip link show` succeeds. In practice the host mlx5_core recovers
	// on its own within ~10-30s of the DPU completing its second boot;
	// we just have to wait for it.
	if !f.skipBF3Settle && j.host.DataPlane != nil && j.host.DataPlane.ParentIface != "" {
		if err := waitForHostParentIface(ctx, repo, j, f.bf3SettleTimeout, w); err != nil {
			fmt.Fprintf(w, "      WARN: parent_iface settle wait did not converge: %v\n", err)
			fmt.Fprintln(w, "      `host network setup` will attempt mlx5_core reload to recover.")
		}
	}

	journaled(journalMu, repo, j.hostname, j.dpu, "SUCCESS", logPath, "")
	return nil
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
		cfg.Jumphost = &ssh.Config{
			Address: h.SSH.Jumphost, Port: 22,
			User: h.SSH.User, KeyPath: keyPath,
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

// waitForDPUSSHWithGrace runs the primary wait, and on timeout extends
// once for an additional 30s "grace" before reporting failure. The
// homelab PoC saw both boot waits expire on the millisecond while the
// DPU was actually responding — rshim/cloud-init settling races the
// primary timeout. One extra burst absorbs that without raising the
// primary (which would slow the happy path).
func waitForDPUSSHWithGrace(ctx context.Context, host *ssh.Client, dpuIP string, primary time.Duration) error {
	first, fcancel := context.WithTimeout(ctx, primary)
	err := waitForDPUSSH(first, host, dpuIP)
	fcancel()
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return err
	}
	grace, gcancel := context.WithTimeout(ctx, 30*time.Second)
	defer gcancel()
	return waitForDPUSSH(grace, host, dpuIP)
}

// waitForDPUSSH polls TCP/22 on dpuIP from the host every 5 s.
func waitForDPUSSH(ctx context.Context, host *ssh.Client, dpuIP string) error {
	cmd := fmt.Sprintf("timeout 4 bash -c '</dev/tcp/%s/22' && echo open || echo closed", dpuIP)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		r := host.Run(ctx, cmd)
		if r.OK() && strings.TrimSpace(r.Stdout) == "open" {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

func appendFlashJournal(repo, hostname string, dpu *poc.DPU, status, logPath, errMsg string) {
	date := time.Now().UTC().Format("2006-01-02")
	path := filepath.Join(repo, "journal", date+"-provision.md")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "## lab-tech: flash DPU %s on %s — %s\n", dpu.PCI, hostname, status)
	fmt.Fprintf(f, "- Time: %s\n", time.Now().UTC().Format(time.RFC3339))
	fmt.Fprintf(f, "- DPU: pci=%s mode=%s lag=%v hostname=%s tmfifo=%s\n",
		dpu.PCI, orDash(dpu.Mode), dpu.LAG, orDash(dpu.Hostname), orDash(dpu.TmfifoIP))
	fmt.Fprintf(f, "- bfb-install log: %s\n", strings.TrimPrefix(logPath, repo+string(filepath.Separator)))
	if errMsg != "" {
		fmt.Fprintf(f, "- Error: %s\n", errMsg)
	}
	fmt.Fprintln(f, "- Next: pre-sales SE confirms DPU reachability + decides cluster phase")
	fmt.Fprintln(f)
}
