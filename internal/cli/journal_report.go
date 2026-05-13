package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mwiget/dpubnkctl/internal/poc"
)

func newJournalReportCmd() *cobra.Command {
	var (
		pocDir string
		out    string
	)
	cmd := &cobra.Command{
		Use:   "report",
		Short: "Render the lessons-learned report from journal entries + decisions.md + poc.yaml",
		Long: `Aggregate every journal/<date>-<phase>.md entry, the decisions.md
running log, and poc.yaml.status into a single markdown report:

  - Executive summary       (PoC name, customer, date range, final status)
  - Topology recap          (hosts, control planes, DPUs, LAG layout)
  - Phase-by-phase timeline (chronological, each entry's leading heading)
  - Decisions               (verbatim copy of decisions.md)
  - Issues encountered      (FAILED entries grouped)
  - Outcome + recommendations (template section the SE fills in)

Default output: <poc>/journal/<YYYY-MM-DD>-final-report.md (mode 0644).
--out - writes to stdout instead (for piping into a renderer / pager).

The report is idempotent: running it twice overwrites the previous file
with the same content if nothing under journal/ or decisions.md changed.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runJournalReport(cmd.OutOrStdout(), pocDir, out)
		},
	}
	cmd.Flags().StringVar(&pocDir, "poc", "", "PoC repo path (default: current directory)")
	cmd.Flags().StringVar(&out, "out", "", "Output path (default: journal/<date>-final-report.md, '-' for stdout)")
	return cmd
}

func runJournalReport(w io.Writer, pocDir, outPath string) error {
	repo, err := resolvePoCDir(pocDir)
	if err != nil {
		return err
	}
	p, err := poc.Load(repo)
	if err != nil {
		return fmt.Errorf("not a PoC repo (%s): %w", repo, err)
	}

	report, err := renderJournalReport(repo, p)
	if err != nil {
		return err
	}

	if outPath == "-" {
		_, err := io.WriteString(w, report)
		return err
	}
	if outPath == "" {
		outPath = filepath.Join(repo, "journal",
			time.Now().UTC().Format("2006-01-02")+"-final-report.md")
	}
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(outPath, []byte(report), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(w, "wrote %s (%d bytes)\n", strings.TrimPrefix(outPath, repo+string(filepath.Separator)), len(report))
	return nil
}

// renderJournalReport builds the report text. Pure function modulo
// file reads — used by tests via a tempdir.
func renderJournalReport(repo string, p *poc.PoC) (string, error) {
	var b strings.Builder

	// --- header ---
	fmt.Fprintf(&b, "# PoC report — %s\n\n", p.Metadata.Name)
	if p.Metadata.Customer != "" {
		fmt.Fprintf(&b, "**Customer:** %s  \n", p.Metadata.Customer)
	}
	fmt.Fprintf(&b, "**Generated:** %s  \n", time.Now().UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "**dpubnkctl:** %s (BNK %s)\n\n", p.Metadata.DpubnkctlVersion, p.Metadata.BNKVersion)

	// --- executive summary ---
	b.WriteString("## Executive summary\n\n")
	fmt.Fprintf(&b, "| Phase | Status |\n|---|---|\n")
	fmt.Fprintf(&b, "| discover  | %s |\n", statusOrPending(p.Status.Discover))
	fmt.Fprintf(&b, "| provision | %s |\n", statusOrPending(p.Status.Provision))
	fmt.Fprintf(&b, "| cluster   | %s |\n", statusOrPending(p.Status.Cluster))
	fmt.Fprintf(&b, "| deploy    | %s |\n", statusOrPending(p.Status.Deploy))
	if !p.Status.LastPhaseAt.IsZero() {
		fmt.Fprintf(&b, "\nLast phase at: %s\n", p.Status.LastPhaseAt.UTC().Format(time.RFC3339))
	}
	b.WriteString("\n")

	// --- topology recap ---
	b.WriteString("## Topology\n\n")
	cps, workers := 0, 0
	for _, h := range p.Hosts {
		switch h.Role {
		case "control-plane":
			cps++
		case "worker":
			workers++
		case "both":
			cps++
			workers++
		}
	}
	fmt.Fprintf(&b, "- Hosts: %d   (control planes: %d, workers: %d)\n", len(p.Hosts), cps, workers)
	if p.Network.ClusterAPIServerAddress != "" {
		fmt.Fprintf(&b, "- Cluster apiserver address: `%s`\n", p.Network.ClusterAPIServerAddress)
	}
	fmt.Fprintf(&b, "- DPU MTU / Pod MTU: %d / %d\n", p.Network.DPUMTU, p.Network.PodMTU)
	if p.Network.InternalCIDR != "" {
		fmt.Fprintf(&b, "- Pod CIDR: %s\n", p.Network.InternalCIDR)
	}
	dpuCount, lagCount := 0, 0
	for _, h := range p.Hosts {
		for _, d := range h.DPUs {
			dpuCount++
			if d.LAG {
				lagCount++
			}
		}
	}
	if dpuCount > 0 {
		fmt.Fprintf(&b, "- DPUs: %d  (LAG: %d, non-LAG: %d)\n", dpuCount, lagCount, dpuCount-lagCount)
	}
	b.WriteString("\nPer-host:\n\n")
	for _, h := range p.Hosts {
		fmt.Fprintf(&b, "- **%s** (role: %s)\n", h.Name, h.Role)
		for _, d := range h.DPUs {
			lag := "non-LAG"
			if d.LAG {
				lag = "LAG"
			}
			fmt.Fprintf(&b, "  - DPU `%s` (%s, hostname `%s`)\n", d.PCI, lag, d.Hostname)
		}
	}
	b.WriteString("\n")

	// --- phase timeline ---
	b.WriteString("## Phase timeline\n\n")
	entries, err := readJournalEntries(repo)
	if err != nil {
		return "", fmt.Errorf("read journal: %w", err)
	}
	if len(entries) == 0 {
		b.WriteString("_No journal entries recorded yet._\n\n")
	} else {
		for _, e := range entries {
			// Skip the final-report file itself if regenerating.
			if strings.HasSuffix(e.File, "-final-report.md") {
				continue
			}
			body, err := os.ReadFile(filepath.Join(repo, e.File))
			if err != nil {
				return "", fmt.Errorf("read %s: %w", e.File, err)
			}
			fmt.Fprintf(&b, "### %s — %s\n\n", e.Date, e.Phase)
			b.WriteString(indentBlock(string(body), "    "))
			b.WriteString("\n")
		}
	}

	// --- decisions verbatim ---
	b.WriteString("## Decisions\n\n")
	decPath := filepath.Join(repo, "decisions.md")
	if dec, err := os.ReadFile(decPath); err == nil {
		// Avoid double-titling: strip a leading `# Decisions —` heading if
		// present so the section header isn't repeated.
		b.WriteString(stripLeadingH1(string(dec)))
		b.WriteString("\n")
	} else {
		b.WriteString("_No decisions.md found._\n\n")
	}

	// --- issues encountered ---
	b.WriteString("## Issues encountered\n\n")
	issues := collectFailureLines(repo, entries)
	if len(issues) == 0 {
		b.WriteString("_No FAILED entries recorded._\n\n")
	} else {
		for _, line := range issues {
			fmt.Fprintf(&b, "- %s\n", line)
		}
		b.WriteString("\n")
	}

	// --- outcome template ---
	b.WriteString("## Outcome and recommendations\n\n")
	b.WriteString("_To be filled in by the pre-sales SE before customer handoff._\n\n")
	b.WriteString("- Did the customer accept the PoC outcome? (yes / partial / no)\n")
	b.WriteString("- Next step the customer agreed to (production rollout, expanded PoC, paused, …)\n")
	b.WriteString("- Lessons learned that should feed back into the dpubnkctl AGENTS.md gotchas list\n")
	b.WriteString("- Open issues handed to engineering / TAM\n\n")

	return b.String(), nil
}

func statusOrPending(s string) string {
	if s == "" {
		return "pending"
	}
	return s
}

// indentBlock prefixes every line of s with prefix. Used to nest a
// journal file's full body inside the report's phase heading.
func indentBlock(s, prefix string) string {
	var out strings.Builder
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		out.WriteString(prefix)
		out.WriteString(sc.Text())
		out.WriteByte('\n')
	}
	return out.String()
}

// stripLeadingH1 removes a single `# …` heading + the blank line after,
// if the input starts with one. The report has its own "## Decisions"
// section heading; this avoids stacking two titles when the file starts
// with `# Decisions — <name>`.
func stripLeadingH1(s string) string {
	lines := strings.SplitN(s, "\n", 3)
	if len(lines) >= 1 && strings.HasPrefix(strings.TrimSpace(lines[0]), "# ") {
		if len(lines) >= 2 && strings.TrimSpace(lines[1]) == "" {
			if len(lines) == 3 {
				return lines[2]
			}
			return ""
		}
		return strings.Join(lines[1:], "\n")
	}
	return s
}

// collectFailureLines scans every journal entry for lines that look like
// recorded failures and returns them prefixed with their source file.
// Pattern is loose on purpose: the auto-journal writers (cluster_up.go,
// provision_dpu.go, deploy_*.go, destroy.go) use "FAILED" in their
// status headings — that's the consistent marker.
func collectFailureLines(repo string, entries []journalEntry) []string {
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.File, "-final-report.md") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(repo, e.File))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(strings.NewReader(string(body)))
		for sc.Scan() {
			line := sc.Text()
			if strings.Contains(line, "FAILED") {
				out = append(out, fmt.Sprintf("`%s`: %s", e.File, strings.TrimSpace(line)))
			}
		}
	}
	sort.Strings(out)
	return out
}
