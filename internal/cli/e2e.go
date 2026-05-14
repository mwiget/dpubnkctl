package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mwiget/dpubnkctl/internal/poc"
	"github.com/mwiget/dpubnkctl/internal/version"
)

// e2ePhase is one orchestrated step in the autonomous deploy sequence.
// args are appended to the dpubnkctl invocation; confirmFlag/confirmVal
// are filled in dynamically when destructive=true so the per-phase
// safety gates (--yolo --confirm-cluster NAME, etc.) don't have to be
// hand-typed in a config file.
type e2ePhase struct {
	name        string
	subcmd      []string                // verb path: ["cluster", "up"]
	args        []string                // extra static args
	destructive bool                    // append --yolo + confirmFlag
	confirmFlag string                  // "--confirm-cluster" | "--confirm-flash" | "--confirm-deploy"
	confirmVal  func(*poc.PoC) string   // computes the value
	preFlight   func(*poc.PoC) (string, bool) // returns (skip-reason, true) to skip the phase
	positional  func(*poc.PoC) []string // positional args (e.g. host list for provision)
}

// canonicalPhases is the full pipeline in deploy order. Operators can
// select a subset via --phase <comma-list> matching .name.
var canonicalPhases = []e2ePhase{
	{
		name:   "validate",
		subcmd: []string{"validate"},
	},
	{
		name:        "provision",
		subcmd:      []string{"provision", "dpu"},
		destructive: true,
		confirmFlag: "--confirm-flash",
		confirmVal:  hostListWithDPUs,
		positional:  hostListWithDPUsSlice,
		preFlight: func(p *poc.PoC) (string, bool) {
			if hostListWithDPUs(p) == "" {
				return "no hosts in poc.yaml have DPUs", true
			}
			return "", false
		},
	},
	{
		name:        "host-network",
		subcmd:      []string{"host", "network", "setup"},
		destructive: true,
		confirmFlag: "--confirm-cluster",
		confirmVal:  pocName,
	},
	{
		name:        "cluster-up",
		subcmd:      []string{"cluster", "up"},
		destructive: true,
		confirmFlag: "--confirm-cluster",
		confirmVal:  pocName,
	},
	{
		name:        "cluster-join-dpus",
		subcmd:      []string{"cluster", "join-dpus"},
		destructive: true,
		confirmFlag: "--confirm-cluster",
		confirmVal:  pocName,
	},
	{
		name:        "deploy-network",
		subcmd:      []string{"deploy", "network"},
		destructive: true,
		confirmFlag: "--confirm-deploy",
		confirmVal:  pocName,
	},
	{
		name:        "deploy-flo",
		subcmd:      []string{"deploy", "flo"},
		destructive: true,
		confirmFlag: "--confirm-deploy",
		confirmVal:  pocName,
	},
	{
		name:        "deploy-cne",
		subcmd:      []string{"deploy", "cne"},
		destructive: true,
		confirmFlag: "--confirm-deploy",
		confirmVal:  pocName,
	},
}

func pocName(p *poc.PoC) string { return p.Metadata.Name }

// hostListWithDPUs returns the comma-separated list of host names that
// have at least one DPU — the value `provision dpus --confirm-flash`
// expects and also the positional argument list it takes.
func hostListWithDPUs(p *poc.PoC) string {
	return strings.Join(hostListWithDPUsSlice(p), ",")
}

func hostListWithDPUsSlice(p *poc.PoC) []string {
	var out []string
	for _, h := range p.Hosts {
		if len(h.DPUs) > 0 {
			out = append(out, h.Name)
		}
	}
	return out
}

type e2eFlags struct {
	pocDir            string
	phaseFilter       string
	reportDir         string
	yolo              bool
	dryRun            bool
	continueOnFailure bool
	skipValidate      bool
	noResume          bool
}

func newE2ECmd() *cobra.Command {
	f := &e2eFlags{}
	cmd := &cobra.Command{
		Use:   "e2e",
		Short: "Drive the full deploy pipeline end-to-end against a populated PoC repo",
		Long: `Run every phase from validate through deploy cne in order,
auto-filling the --yolo and --confirm-* safety gates from poc.yaml. Each
phase's stdout/stderr lands at reports/<timestamp>/logs/NN-<phase>.log;
per-phase results aggregate into reports/<timestamp>/run.json and
reports/<timestamp>/run.md.

Canonical phase order (override with --phase <comma-list> to run a subset):

  validate           dpubnkctl validate
  provision          dpubnkctl provision dpu <hosts> --yolo --confirm-flash <hosts>
  host-network       dpubnkctl host network setup --yolo --confirm-cluster <name>
  cluster-up         dpubnkctl cluster up --yolo --confirm-cluster <name>
  cluster-join-dpus  dpubnkctl cluster join-dpus --yolo --confirm-cluster <name>
  deploy-network     dpubnkctl deploy network --yolo --confirm-deploy <name>
  deploy-flo         dpubnkctl deploy flo --yolo --confirm-deploy <name>
  deploy-cne         dpubnkctl deploy cne --yolo --confirm-deploy <name>

Prerequisites:
  - poc.yaml has hosts[] populated (run dpubnkctl discover wizard first,
    or crib an examples/*.yaml shape).
  - keys/ has FAR tarball, JWT, DPU password hash, and the SSH key each
    host's ssh.key_ref points at.
  - dpubnkctl validate is clean (this is checked as phase 1).

DESTRUCTIVE. Re-flashes DPUs, brings up the cluster, deploys BNK. The
full pipeline typically takes 60–90 minutes against real hardware.

Invocation summary:

  dpubnkctl e2e                  Print the plan + how to proceed (no-op).
  dpubnkctl e2e --dry-run        Print the plan with exact per-phase
                                 invocations. Runs nothing.
  dpubnkctl e2e --yolo           Actually run the pipeline. Resume-safe
                                 — already-completed phases skipped via
                                 artifacts/e2e-state.json.
  dpubnkctl e2e --yolo --no-resume     Re-run every phase from scratch.
  dpubnkctl e2e --yolo --phase A,B,C   Run only the listed phases.
  dpubnkctl e2e --yolo --continue-on-failure
                                 Keep going past a failed phase (useful
                                 for diagnosing without restarting).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runE2E(cmd.Context(), cmd.OutOrStdout(), f)
		},
	}
	cmd.Flags().StringVar(&f.pocDir, "poc", "", "PoC repo path (default: current directory)")
	cmd.Flags().StringVar(&f.phaseFilter, "phase", "", "Comma-separated subset of phases to run (default: all in canonical order)")
	cmd.Flags().StringVar(&f.reportDir, "report-dir", "", "Output dir (default: <poc>/reports/<RFC3339-timestamp>/)")
	cmd.Flags().BoolVar(&f.yolo, "yolo", false, "Acknowledge the pipeline is destructive and actually run it (required; without it e2e prints the plan and exits)")
	cmd.Flags().BoolVar(&f.dryRun, "dry-run", false, "Print the plan, run nothing")
	cmd.Flags().BoolVar(&f.continueOnFailure, "continue-on-failure", false, "Keep running phases after a failure")
	cmd.Flags().BoolVar(&f.skipValidate, "skip-validate", false, "Skip the validate precheck (not recommended)")
	cmd.Flags().BoolVar(&f.noResume, "no-resume", false, "Ignore artifacts/e2e-state.json and re-run every phase from scratch")
	return cmd
}

// e2eState persists per-phase completion across e2e runs so an
// interrupted (or partially-failed-with-continue) pipeline can resume
// without re-running phases that already succeeded. Lives at
// <poc>/artifacts/e2e-state.json. validate is intentionally NOT
// recorded — it's cheap and re-running it is the right default.
type e2eState struct {
	Phases map[string]e2ePhaseState `json:"phases"`
}

type e2ePhaseState struct {
	Status      string    `json:"status"` // ok | failed
	CompletedAt time.Time `json:"completed_at"`
	Duration    string    `json:"duration,omitempty"`
}

func loadE2EState(repo string) e2eState {
	s := e2eState{Phases: map[string]e2ePhaseState{}}
	path := filepath.Join(repo, "artifacts", "e2e-state.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	_ = json.Unmarshal(data, &s)
	if s.Phases == nil {
		s.Phases = map[string]e2ePhaseState{}
	}
	return s
}

func saveE2EState(repo string, s e2eState) error {
	dir := filepath.Join(repo, "artifacts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "e2e-state.json"), data, 0o644)
}

func runE2E(ctx context.Context, out io.Writer, f *e2eFlags) error {
	repo, err := resolvePoCDir(f.pocDir)
	if err != nil {
		return err
	}
	p, err := poc.Load(repo)
	if err != nil {
		return fmt.Errorf("not a PoC repo (%s): %w", repo, err)
	}

	selected, err := selectPhases(f.phaseFilter, f.skipValidate)
	if err != nil {
		return err
	}

	binary, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate dpubnkctl binary: %w", err)
	}

	// Discoverability gate: a bare `dpubnkctl e2e` used to start the
	// full destructive pipeline immediately, which surprised operators
	// (and agents) running the binary for the first time. Require an
	// explicit --yolo or --dry-run; otherwise print what the run will
	// do, point at the relevant flags, and exit cleanly so the operator
	// can decide.
	if !f.yolo && !f.dryRun {
		printE2EPlan(out, p, repo, binary, selected, f)
		return nil
	}

	reportDir := f.reportDir
	if reportDir == "" {
		reportDir = filepath.Join(repo, "reports", time.Now().UTC().Format("2006-01-02T15-04-05Z"))
	}
	logDir := filepath.Join(reportDir, "logs")
	if !f.dryRun {
		if err := os.MkdirAll(logDir, 0o755); err != nil {
			return err
		}
	}

	fmt.Fprintf(out, "PoC: %s   (BNK %s)\n", p.Metadata.Name, p.Metadata.BNKVersion)
	fmt.Fprintf(out, "Phases (%d): %s\n", len(selected), phaseNames(selected))
	if !f.dryRun {
		fmt.Fprintf(out, "Reports: %s\n", reportDir)
	}
	fmt.Fprintln(out)

	report := runReport{
		StartedAt: time.Now().UTC(),
		PoCName:   p.Metadata.Name,
		Phases:    nil,
	}

	state := loadE2EState(repo)
	if f.noResume {
		state = e2eState{Phases: map[string]e2ePhaseState{}}
	}

	for i, ph := range selected {
		idx := i + 1
		stepName := fmt.Sprintf("%02d-%s", idx, ph.name)

		// preFlight skip (e.g. "no hosts with DPUs") trumps everything.
		if reason, skip := phaseSkip(p, ph); skip {
			fmt.Fprintf(out, "[%d/%d] %-22s  SKIPPED — %s\n", idx, len(selected), ph.name, reason)
			report.Phases = append(report.Phases, phaseReport{
				Phase: ph.name, Status: "skipped", Summary: reason, Index: idx,
			})
			continue
		}

		// Resume skip — previously-completed phase, no --no-resume override.
		// validate is intentionally always re-run (cheap, catches drift).
		if !f.dryRun && ph.name != "validate" {
			if prev, ok := state.Phases[ph.name]; ok && prev.Status == "ok" {
				reason := fmt.Sprintf("resumed: previously completed at %s (--no-resume to re-run)",
					prev.CompletedAt.Format(time.RFC3339))
				fmt.Fprintf(out, "[%d/%d] %-22s  SKIPPED — %s\n", idx, len(selected), ph.name, reason)
				report.Phases = append(report.Phases, phaseReport{
					Phase: ph.name, Status: "skipped", Summary: reason, Index: idx,
				})
				continue
			}
		}

		args := buildArgs(p, repo, ph)
		shown := binary + " " + strings.Join(args, " ")
		fmt.Fprintf(out, "[%d/%d] %s\n      %s\n", idx, len(selected), ph.name, shown)

		if f.dryRun {
			report.Phases = append(report.Phases, phaseReport{
				Phase: ph.name, Status: "dry-run", Summary: shown, Index: idx,
			})
			continue
		}

		started := time.Now()
		logPath := filepath.Join(logDir, stepName+".log")
		exit, err := runOnePhase(ctx, binary, args, logPath)
		dur := time.Since(started)
		rep := phaseReport{
			Phase:     ph.name,
			Index:     idx,
			StartedAt: started.UTC(),
			Duration:  dur.Truncate(time.Second).String(),
			ExitCode:  exit,
			LogPath:   "logs/" + stepName + ".log",
			Command:   shown,
		}
		switch {
		case err != nil && exit < 0:
			rep.Status = "failed"
			rep.Summary = "transport error: " + err.Error()
		case exit != 0:
			rep.Status = "failed"
			rep.Summary = fmt.Sprintf("exit %d (see %s)", exit, rep.LogPath)
		default:
			rep.Status = "ok"
			rep.Summary = fmt.Sprintf("completed in %s", rep.Duration)
		}
		fmt.Fprintf(out, "      %s  (%s, %s)\n\n", strings.ToUpper(rep.Status), rep.Duration, rep.LogPath)
		report.Phases = append(report.Phases, rep)

		// Persist per-phase state so the next run can resume past this
		// point. validate skipped (always re-run); failed phases are
		// recorded too so the operator can see what flipped to failed.
		if ph.name != "validate" {
			state.Phases[ph.name] = e2ePhaseState{
				Status:      rep.Status,
				CompletedAt: time.Now().UTC(),
				Duration:    rep.Duration,
			}
			if err := saveE2EState(repo, state); err != nil {
				fmt.Fprintf(out, "      WARN: could not persist e2e state: %v\n", err)
			}
		}

		if rep.Status == "failed" && !f.continueOnFailure {
			break
		}
	}

	report.FinishedAt = time.Now().UTC()
	if f.dryRun {
		return nil
	}
	if err := writeRunReports(reportDir, report, p); err != nil {
		fmt.Fprintf(out, "WARN: could not write reports: %v\n", err)
	}

	failed := 0
	for _, ph := range report.Phases {
		if ph.Status == "failed" {
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("e2e: %d phase(s) failed — see %s", failed, reportDir)
	}
	fmt.Fprintf(out, "DONE. Report at %s\n", reportDir)
	return nil
}

// printE2EPlan summarises what `dpubnkctl e2e --yolo` would do for this
// PoC and exits without running. Shows the actual per-phase
// subcommands that would fire (gates auto-filled from poc.yaml) so the
// operator can preview before opting in.
func printE2EPlan(out io.Writer, p *poc.PoC, repo, binary string, selected []e2ePhase, f *e2eFlags) {
	fmt.Fprintf(out, "PoC: %s   (BNK %s)\n", p.Metadata.Name, p.Metadata.BNKVersion)
	fmt.Fprintf(out, "Repo: %s\n\n", repo)
	fmt.Fprintln(out, "`dpubnkctl e2e` runs the full deploy pipeline end-to-end. It is")
	fmt.Fprintln(out, "DESTRUCTIVE (BFB re-flash, kubespray cluster bring-up, BNK deploy)")
	fmt.Fprintln(out, "and typically takes 60–90 minutes on a 2-host PoC.")
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Phases (%d) — would run, in order:\n\n", len(selected))
	for i, ph := range selected {
		args := buildArgs(p, repo, ph)
		fmt.Fprintf(out, "  %d. %-18s %s %s\n", i+1, ph.name, binary, strings.Join(args, " "))
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Nothing has been changed. To proceed:")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "  dpubnkctl e2e --yolo               # actually run (resume-safe)")
	fmt.Fprintln(out, "  dpubnkctl e2e --yolo --no-resume   # re-run every phase from scratch")
	fmt.Fprintln(out, "  dpubnkctl e2e --dry-run            # show the plan again (no-op)")
	fmt.Fprintln(out, "  dpubnkctl validate                 # sanity-check poc.yaml first")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Reports + per-phase logs land under reports/<RFC3339-timestamp>/.")
}

func selectPhases(filter string, skipValidate bool) ([]e2ePhase, error) {
	if filter == "" {
		if skipValidate {
			return canonicalPhases[1:], nil
		}
		return canonicalPhases, nil
	}
	want := map[string]bool{}
	for _, n := range strings.Split(filter, ",") {
		want[strings.TrimSpace(n)] = true
	}
	var out []e2ePhase
	for _, ph := range canonicalPhases {
		if want[ph.name] {
			out = append(out, ph)
			delete(want, ph.name)
		}
	}
	if len(want) > 0 {
		var unknown []string
		for k := range want {
			unknown = append(unknown, k)
		}
		sort.Strings(unknown)
		return nil, fmt.Errorf("unknown phase(s): %s (valid: %s)", strings.Join(unknown, ", "), phaseNames(canonicalPhases))
	}
	return out, nil
}

func phaseNames(phs []e2ePhase) string {
	names := make([]string, len(phs))
	for i, ph := range phs {
		names[i] = ph.name
	}
	return strings.Join(names, ", ")
}

func phaseSkip(p *poc.PoC, ph e2ePhase) (string, bool) {
	if ph.preFlight == nil {
		return "", false
	}
	return ph.preFlight(p)
}

func buildArgs(p *poc.PoC, repo string, ph e2ePhase) []string {
	args := append([]string{}, ph.subcmd...)
	if ph.positional != nil {
		args = append(args, ph.positional(p)...)
	}
	args = append(args, "--poc", repo)
	args = append(args, ph.args...)
	if ph.destructive {
		args = append(args, "--yolo")
		if ph.confirmFlag != "" && ph.confirmVal != nil {
			args = append(args, ph.confirmFlag, ph.confirmVal(p))
		}
	}
	return args
}

func runOnePhase(ctx context.Context, binary string, args []string, logPath string) (int, error) {
	f, err := os.Create(logPath)
	if err != nil {
		return -1, err
	}
	defer f.Close()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Stdout = f
	cmd.Stderr = f
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode(), nil
		}
		return -1, err
	}
	return 0, nil
}

type runReport struct {
	PoCName    string        `json:"poc_name"`
	StartedAt  time.Time     `json:"started_at"`
	FinishedAt time.Time     `json:"finished_at"`
	Phases     []phaseReport `json:"phases"`
}

type phaseReport struct {
	Index     int       `json:"index"`
	Phase     string    `json:"phase"`
	Status    string    `json:"status"` // ok | failed | skipped | dry-run
	StartedAt time.Time `json:"started_at,omitempty"`
	Duration  string    `json:"duration,omitempty"`
	ExitCode  int       `json:"exit_code,omitempty"`
	LogPath   string    `json:"log_path,omitempty"`
	Command   string    `json:"command,omitempty"`
	Summary   string    `json:"summary"`
}

func writeRunReports(dir string, r runReport, p *poc.PoC) error {
	jsonBytes, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "run.json"), jsonBytes, 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "run.md"), []byte(renderRunMarkdown(r, p)), 0o644)
}

func renderRunMarkdown(r runReport, p *poc.PoC) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# e2e report — %s\n\n", r.PoCName)
	fmt.Fprintf(&b, "- **Started:** %s\n", r.StartedAt.Format(time.RFC3339))
	fmt.Fprintf(&b, "- **Finished:** %s\n", r.FinishedAt.Format(time.RFC3339))
	fmt.Fprintf(&b, "- **Wall:** %s\n\n", r.FinishedAt.Sub(r.StartedAt).Truncate(time.Second))
	ok, failed, skipped := 0, 0, 0
	for _, ph := range r.Phases {
		switch ph.Status {
		case "ok":
			ok++
		case "failed":
			failed++
		case "skipped":
			skipped++
		}
	}
	fmt.Fprintf(&b, "**Result:** %d ok, %d failed, %d skipped\n\n", ok, failed, skipped)

	// Pinned versions from this dpubnkctl release. Cluster status pulls
	// the live deployed values; for an e2e report the binary-pinned
	// versions are what actually got applied, so they're what we record.
	fmt.Fprintln(&b, "## Versions")
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "- **dpubnkctl:** %s  (targets BNK %s)\n", version.Version, version.BNKVersion)
	fmt.Fprintf(&b, "- **FLO chart:** %s\n", version.FLOChartVer)
	fmt.Fprintf(&b, "- **CNE manifest:** %s\n\n", version.CNEManifestVersion)

	// Embed the topology diagrams so any markdown reader (or grep) sees
	// the cluster shape inline with the results. Wrap in a fenced code
	// block to preserve the ASCII alignment in renderers that reflow
	// plain text.
	if p != nil {
		b.WriteString("## Topology\n\n")
		b.WriteString("```\n")
		b.WriteString(RenderClusterASCII(p))
		b.WriteString("\n")
		b.WriteString(RenderVLANsASCII(p))
		b.WriteString("```\n\n")
	}

	b.WriteString("## Phases\n\n")
	b.WriteString("| # | Phase | Status | Duration | Log |\n")
	b.WriteString("|---|---|---|---|---|\n")
	for _, ph := range r.Phases {
		dur := ph.Duration
		if dur == "" {
			dur = "—"
		}
		logCell := "—"
		if ph.LogPath != "" {
			logCell = "`" + ph.LogPath + "`"
		}
		fmt.Fprintf(&b, "| %d | %s | %s | %s | %s |\n", ph.Index, ph.Phase, statusBadge(ph.Status), dur, logCell)
	}

	b.WriteString("\n## Per-phase summary\n\n")
	for _, ph := range r.Phases {
		fmt.Fprintf(&b, "### %d. %s — %s\n\n", ph.Index, ph.Phase, ph.Status)
		if ph.Command != "" {
			fmt.Fprintf(&b, "    %s\n\n", ph.Command)
		}
		if ph.Summary != "" {
			fmt.Fprintf(&b, "%s\n\n", ph.Summary)
		}
	}
	return b.String()
}

func statusBadge(s string) string {
	switch s {
	case "ok":
		return "✅ ok"
	case "failed":
		return "❌ failed"
	case "skipped":
		return "⏭️ skipped"
	case "dry-run":
		return "🟦 dry-run"
	default:
		return s
	}
}
