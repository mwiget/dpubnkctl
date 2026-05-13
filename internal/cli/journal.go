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

// validPersonas accepted by `journal add --as`. Free-form is also OK —
// these are just the well-known role names from internal/embedded/files/personas/.
var validPersonas = map[string]bool{
	"pre-sales-se":   true,
	"lab-tech":       true,
	"doc-specialist": true,
	"operator":       true, // generic catch-all
}

func newJournalCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "journal",
		Short: "Manage the PoC journal (append-only timeline of phases + events)",
		Long: `The PoC journal lives at <poc>/journal/<date>-<phase>.md and is
appended to automatically by every phase command (discover, provision,
cluster, deploy, destroy). Use this subcommand to:

  list    — show all journal entries with one-line summaries
  add     — append a free-form note (SE observation, customer comment,
            scope shift, anything that doesn't tie to a phase command)
  report  — render the final lessons-learned report (see "journal report")

The journal is the doc-specialist persona's working surface and the
source material for "journal report" at PoC close.`,
	}
	cmd.AddCommand(newJournalListCmd(), newJournalAddCmd(), newJournalReportCmd())
	return cmd
}

// (newJournalReportCmd lives in journal_report.go alongside the renderer.)

// --- journal list ---------------------------------------------------------

func newJournalListCmd() *cobra.Command {
	var pocDir string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List journal entries with one-line summaries (sorted chronologically)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runJournalList(cmd.OutOrStdout(), pocDir)
		},
	}
	cmd.Flags().StringVar(&pocDir, "poc", "", "PoC repo path (default: current directory)")
	return cmd
}

func runJournalList(out io.Writer, pocDir string) error {
	repo, err := resolvePoCDir(pocDir)
	if err != nil {
		return err
	}
	entries, err := readJournalEntries(repo)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Fprintln(out, "no journal entries yet")
		return nil
	}
	for _, e := range entries {
		fmt.Fprintf(out, "%-12s  %-18s  %s\n", e.Date, e.Phase, e.Summary)
	}
	return nil
}

// journalEntry is one row of `journal list` — one row per file under
// <poc>/journal/. Summary is the first non-empty markdown heading found.
type journalEntry struct {
	Date    string // "2026-05-13" — parsed from filename
	Phase   string // "cluster" / "discover" / "deploy" / "notes" / ...
	File    string // path relative to repo
	Summary string // first non-blank heading
}

func readJournalEntries(repo string) ([]journalEntry, error) {
	dir := filepath.Join(repo, "journal")
	infos, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []journalEntry
	for _, fi := range infos {
		if fi.IsDir() || !strings.HasSuffix(fi.Name(), ".md") {
			continue
		}
		// Expect <YYYY-MM-DD>-<phase>.md; tolerate odd shapes.
		date, phase := splitJournalFilename(fi.Name())
		summary := firstHeading(filepath.Join(dir, fi.Name()))
		out = append(out, journalEntry{
			Date:    date,
			Phase:   phase,
			File:    filepath.Join("journal", fi.Name()),
			Summary: summary,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Date != out[j].Date {
			return out[i].Date < out[j].Date
		}
		return out[i].Phase < out[j].Phase
	})
	return out, nil
}

// splitJournalFilename pulls "<YYYY-MM-DD>" off the front and treats the
// rest (minus ".md") as the phase. Filenames that don't follow the
// pattern get the whole stem as the phase and an empty date.
func splitJournalFilename(name string) (date, phase string) {
	stem := strings.TrimSuffix(name, ".md")
	// Filename shape from real journal writers: 2026-05-13-cluster.md
	// Date is the first 10 chars if they parse as YYYY-MM-DD.
	if len(stem) >= 11 && stem[10] == '-' {
		if _, err := time.Parse("2006-01-02", stem[:10]); err == nil {
			return stem[:10], stem[11:]
		}
	}
	return "", stem
}

// firstHeading returns the first non-empty markdown heading (line
// starting with `#`) in the file, with leading `#`s and spaces trimmed.
// Empty if the file has none — used as the summary cell in `journal list`.
func firstHeading(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return "(unreadable)"
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "#") {
			return strings.TrimSpace(strings.TrimLeft(line, "#"))
		}
	}
	return "(no heading)"
}

// --- journal add ----------------------------------------------------------

func newJournalAddCmd() *cobra.Command {
	var (
		pocDir  string
		persona string
	)
	cmd := &cobra.Command{
		Use:   "add <message>",
		Short: "Append a free-form note to today's journal (notes-<date>.md)",
		Long: `Append a single-paragraph note to <poc>/journal/<YYYY-MM-DD>-notes.md.
Each entry is timestamped and tagged with the persona — useful for SE
observations, customer comments, scope shifts, or anything that doesn't
land in a phase-command's auto-journal.

The note lands as a level-2 markdown heading with a timestamp, then the
message body, then a blank line. The notes file is created on first add.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			message := strings.Join(args, " ")
			return runJournalAdd(cmd.OutOrStdout(), pocDir, persona, message)
		},
	}
	cmd.Flags().StringVar(&pocDir, "poc", "", "PoC repo path (default: current directory)")
	cmd.Flags().StringVar(&persona, "as", "operator", "Persona signing the entry (pre-sales-se | lab-tech | doc-specialist | operator | custom)")
	return cmd
}

func runJournalAdd(out io.Writer, pocDir, persona, message string) error {
	repo, err := resolvePoCDir(pocDir)
	if err != nil {
		return err
	}
	// Confirm we're in a PoC repo (poc.yaml loads) so accidental `journal
	// add` calls from $HOME don't litter the filesystem.
	if _, err := poc.Load(repo); err != nil {
		return fmt.Errorf("not a PoC repo (%s): %w — cd into one or pass --poc <dir>", repo, err)
	}
	if !validPersonas[persona] {
		// Tolerate custom personas — just print a hint.
		fmt.Fprintf(out, "(persona %q is not a known role; entry recorded anyway)\n", persona)
	}
	now := time.Now().UTC()
	dir := filepath.Join(repo, "journal")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, now.Format("2006-01-02")+"-notes.md")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := fmt.Fprintf(f, "## %s: %s\n%s\n\n",
		persona, now.Format(time.RFC3339), strings.TrimSpace(message)); err != nil {
		return err
	}
	fmt.Fprintf(out, "appended to %s\n", strings.TrimPrefix(path, repo+string(filepath.Separator)))
	return nil
}
