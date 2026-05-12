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
	"time"

	"github.com/spf13/cobra"

	"github.com/mwiget/dpubnkctl/internal/poc"
	"github.com/mwiget/dpubnkctl/internal/provision"
	"github.com/mwiget/dpubnkctl/internal/ssh"
)

type provisionDPUFlags struct {
	pocDir         string
	dpuPCI         string
	yolo           bool
	confirmFlash   string
	bfbURL         string
	flashTimeout   time.Duration
	dpuWaitTimeout time.Duration
	skipDPUWait    bool
}

func newProvisionDPUCmd() *cobra.Command {
	f := &provisionDPUFlags{}
	cmd := &cobra.Command{
		Use:   "dpu <hostname>",
		Short: "Flash a DPU end-to-end (DESTRUCTIVE — wipes the DPU OS)",
		Long: `Cache the BFB image on the operator laptop, render bf.conf, push both
to the host, run bfb-install, then wait for the DPU to come back via
tmfifo. This wipes everything currently on the DPU OS.

Required gates:
  --yolo                 acknowledge that this is a destructive flash
  --confirm-flash NAME   must equal <hostname> (typo guard)

The destructive step does not run unless both gates are present and the
plan is READY. Streaming output is mirrored to artifacts/<host>-flash.log.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runProvisionDPU(cmd.Context(), cmd.OutOrStdout(), args[0], f)
		},
	}
	cmd.Flags().StringVar(&f.pocDir, "poc", "", "PoC repo path (default: current directory)")
	cmd.Flags().StringVar(&f.dpuPCI, "dpu", "", "DPU PCI address (default: first DPU on the host)")
	cmd.Flags().BoolVar(&f.yolo, "yolo", false, "Acknowledge that this command is destructive")
	cmd.Flags().StringVar(&f.confirmFlash, "confirm-flash", "", "Must equal <hostname> — a typo guard for destructive ops")
	cmd.Flags().StringVar(&f.bfbURL, "bfb-url", "", "Override the BFB download URL (defaults to binary-pinned)")
	cmd.Flags().DurationVar(&f.flashTimeout, "flash-timeout", 25*time.Minute, "Per-flash bfb-install timeout")
	cmd.Flags().DurationVar(&f.dpuWaitTimeout, "dpu-wait-timeout", 10*time.Minute, "How long to wait for the DPU to come back via tmfifo after flash")
	cmd.Flags().BoolVar(&f.skipDPUWait, "skip-dpu-wait", false, "Don't wait for the DPU to reboot — return as soon as bfb-install exits")
	return cmd
}

func runProvisionDPU(ctx context.Context, out io.Writer, hostname string, f *provisionDPUFlags) error {
	repo, err := resolvePoCDir(f.pocDir)
	if err != nil {
		return err
	}
	p, err := poc.Load(repo)
	if err != nil {
		return fmt.Errorf("not a PoC repo (%s): %w", repo, err)
	}
	host, err := findHost(p, hostname)
	if err != nil {
		return err
	}
	dpu, err := pickDPU(host, f.dpuPCI)
	if err != nil {
		return err
	}

	if !f.yolo {
		return errors.New("refusing destructive flash without --yolo")
	}
	if f.confirmFlash != hostname {
		return fmt.Errorf("--confirm-flash must equal the hostname argument %q (typo guard)", hostname)
	}

	fmt.Fprintf(out, "PoC:    %s   (BNK %s, DOCA %s)\n", p.Metadata.Name, p.Metadata.BNKVersion, p.Versions.DOCA)
	fmt.Fprintf(out, "Host:   %s   (%s@%s)\n", host.Name, host.SSH.User, host.SSH.Address)
	fmt.Fprintf(out, "DPU:    %s   mode=%s lag=%v hostname=%s tmfifo=%s\n",
		dpu.PCI, orDash(dpu.Mode), dpu.LAG, orDash(dpu.Hostname), orDash(dpu.TmfifoIP))
	fmt.Fprintf(out, "Image:  %s   (DOCA %s)\n\n", p.Versions.BFBImage, p.Versions.DOCA)

	// 1. Render bf.conf and run readiness — abort on either failure.
	fmt.Fprintln(out, "[1/6] Rendering bf.conf ...")
	rendered, err := provision.Render(p, host, dpu, repo)
	if err != nil {
		return fmt.Errorf("bf.conf render: %w", err)
	}
	fmt.Fprintf(out, "      ok — %d bytes (%s)\n", len(rendered), lagTag(dpu.LAG))

	fmt.Fprintln(out, "[2/6] Connecting to host ...")
	cfg, err := sshConfigForHost(repo, host, 30*time.Second)
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
	fmt.Fprintln(out, "      ok")

	fmt.Fprintln(out, "[3/6] Running readiness checks ...")
	probeCtx, pcancel := context.WithTimeout(ctx, 60*time.Second)
	rep := provision.Check(probeCtx, host.Name, provision.AsRunner(client), dpu.PCI)
	pcancel()
	if !rep.Ready() {
		for _, e := range rep.Errors {
			fmt.Fprintln(out, "      err:", e)
		}
		return errors.New("readiness check failed — refusing to flash; run `dpubnkctl provision plan` for details")
	}
	for _, w := range rep.Warnings {
		fmt.Fprintln(out, "      warn:", w)
	}
	fmt.Fprintln(out, "      ok")

	// 2. BFB image cache.
	fmt.Fprintf(out, "[4/6] BFB cache (%s) ...\n", p.Versions.BFBImage)
	bfbProgress := func(written, total int64) {
		if total > 0 {
			fmt.Fprintf(out, "      downloading: %d / %d MiB (%.1f%%)\n", written>>20, total>>20, float64(written)/float64(total)*100)
		} else {
			fmt.Fprintf(out, "      downloaded: %d MiB\n", written>>20)
		}
	}
	bfbPath, err := provision.EnsureBFB(ctx, p.Provisioning.BFBCacheDir, p.Versions.BFBImage, p.Versions.BFBURL, bfbProgress)
	if err != nil {
		return fmt.Errorf("bfb cache: %w", err)
	}
	if st, _ := os.Stat(bfbPath); st != nil {
		fmt.Fprintf(out, "      cached at %s (%d MiB)\n", bfbPath, st.Size()>>20)
	}

	// 3. Push BFB + bf.conf to host /tmp.
	fmt.Fprintln(out, "[5/6] Pushing BFB + bf.conf to host /tmp ...")
	remoteBFB := "/tmp/" + p.Versions.BFBImage
	remoteCfg := "/tmp/dpubnkctl-bf.cfg"

	pushCtx, pushCancel := context.WithTimeout(ctx, 15*time.Minute)
	pushProgress := func(written, total int64) {
		if total > 0 {
			fmt.Fprintf(out, "      sftp: %d / %d MiB (%.1f%%)\n", written>>20, total>>20, float64(written)/float64(total)*100)
		}
	}
	if err := client.PushFile(pushCtx, bfbPath, remoteBFB, pushProgress); err != nil {
		pushCancel()
		return fmt.Errorf("push bfb: %w", err)
	}
	if err := client.PushBytes(pushCtx, []byte(rendered), remoteCfg); err != nil {
		pushCancel()
		return fmt.Errorf("push bf.conf: %w", err)
	}
	pushCancel()
	fmt.Fprintf(out, "      pushed %s + %s\n", remoteBFB, remoteCfg)

	// 4. Run bfb-install with streaming output mirrored to a log file.
	fmt.Fprintln(out, "[6/6] Running bfb-install (this typically takes 5–15 minutes) ...")
	logPath := filepath.Join(repo, "artifacts", host.Name+"-flash.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return err
	}
	logFile, err := os.Create(logPath)
	if err != nil {
		return err
	}
	defer logFile.Close()
	tee := io.MultiWriter(prefixWriter{w: out, prefix: "      | "}, logFile)

	flashCmd := fmt.Sprintf("sudo -n bfb-install --bfb '%s' --config '%s' --rshim rshim0 2>&1", remoteBFB, remoteCfg)
	flashCtx, flashCancel := context.WithTimeout(ctx, f.flashTimeout)
	exit, runErr := client.RunStream(flashCtx, flashCmd, tee)
	flashCancel()
	if runErr != nil {
		appendFlashJournal(repo, host.Name, dpu, "FAILED (transport error)", logPath, runErr.Error())
		return fmt.Errorf("bfb-install: %w", runErr)
	}
	if exit != 0 {
		appendFlashJournal(repo, host.Name, dpu, fmt.Sprintf("FAILED (exit %d)", exit), logPath, "")
		return fmt.Errorf("bfb-install exited %d — see %s", exit, logPath)
	}
	fmt.Fprintf(out, "      bfb-install completed successfully — log at %s\n", logPath)

	// 5. Wait for the DPU to come back via tmfifo (from the host).
	if !f.skipDPUWait {
		dpuIP := tmfifoHostPart(dpu.TmfifoIP)
		if dpuIP == "" {
			fmt.Fprintln(out, "      (no tmfifo_ip set — skipping DPU reachability wait)")
		} else {
			fmt.Fprintf(out, "      waiting for DPU SSH at %s (from host) ...\n", dpuIP)
			waitCtx, waitCancel := context.WithTimeout(ctx, f.dpuWaitTimeout)
			err := waitForDPUSSH(waitCtx, client, dpuIP)
			waitCancel()
			if err != nil {
				fmt.Fprintf(out, "      WARN: DPU did not respond within %s — flash succeeded but verify manually: %v\n", f.dpuWaitTimeout, err)
			} else {
				fmt.Fprintln(out, "      DPU is reachable.")
			}
		}
	}

	// 6. Update poc.yaml status + journal.
	p.Status.Provision = "completed"
	p.Status.LastPhaseAt = time.Now().UTC()
	if err := p.Save(repo); err != nil {
		return err
	}
	appendFlashJournal(repo, host.Name, dpu, "SUCCESS", logPath, "")
	fmt.Fprintln(out, "\nDONE.")
	return nil
}

// prefixWriter prefixes every line of streamed bfb-install output so the
// operator can tell our wrapper messages from the underlying tool's.
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

// tmfifoHostPart returns the IP portion of a CIDR like 192.168.100.2/30.
func tmfifoHostPart(cidr string) string {
	if cidr == "" {
		return ""
	}
	if i := strings.IndexByte(cidr, '/'); i > 0 {
		return cidr[:i]
	}
	return cidr
}

// waitForDPUSSH polls TCP/22 on dpuIP from the host every 5 s.
func waitForDPUSSH(ctx context.Context, host *ssh.Client, dpuIP string) error {
	cmd := fmt.Sprintf("timeout 4 bash -c '</dev/tcp/%s/22' && echo open || echo closed", dpuIP)
	deadline, _ := ctx.Deadline()
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
		_ = deadline
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
