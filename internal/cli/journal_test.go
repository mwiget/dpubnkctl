package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mwiget/dpubnkctl/internal/poc"
)

// makeTestPoC writes a minimal but loadable poc.yaml at dir + the
// directory skeleton (`journal/`, `decisions.md`) so journal commands
// have something to chew on.
func makeTestPoC(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "journal"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := &poc.PoC{
		APIVersion: poc.APIVersion,
		Kind:       poc.Kind,
		Metadata: poc.Metadata{
			Name:     "test-poc",
			Customer: "Acme",
			Created:  time.Now().UTC(),
		},
		Network: poc.Network{
			InternalCIDR: "198.18.100.0/24",
			DPUMTU:       9000,
			PodMTU:       8900,
		},
		Hosts: []poc.Host{{
			Name: "host1", Role: "both",
			SSH:  poc.SSH{Address: "1.2.3.4", User: "u", KeyRef: "keys/k"},
			DPUs: []poc.DPU{{PCI: "0000:03:00.0", LAG: true, Hostname: "host1-bf3"}},
		}},
		Status: poc.Status{Discover: "completed", Cluster: "in_progress"},
	}
	if err := p.Save(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "decisions.md"),
		[]byte("# Decisions — test-poc\n\nFoo decided over bar because Y.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// --- journal list ---------------------------------------------------------

func TestJournalList_Empty(t *testing.T) {
	dir := makeTestPoC(t)
	var buf bytes.Buffer
	if err := runJournalList(&buf, dir); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "no journal entries") {
		t.Errorf("expected empty notice, got %q", buf.String())
	}
}

func TestJournalList_SortsAndSummarizes(t *testing.T) {
	dir := makeTestPoC(t)
	writeFile(t, filepath.Join(dir, "journal", "2026-05-12-init.md"),
		"# PoC initialized\n\nbody body\n")
	writeFile(t, filepath.Join(dir, "journal", "2026-05-13-cluster.md"),
		"## lab-tech: cluster up — SUCCESS\nstuff\n")
	writeFile(t, filepath.Join(dir, "journal", "2026-05-13-deploy.md"),
		"## doc-specialist: deploy flo — SUCCESS\n")

	var buf bytes.Buffer
	if err := runJournalList(&buf, dir); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d:\n%s", len(lines), buf.String())
	}
	// Sort: chronological then by phase. Check date+phase substrings since
	// the column padding is brittle to format-string tweaks.
	wants := []struct{ date, phase string }{
		{"2026-05-12", "init"},
		{"2026-05-13", "cluster"},
		{"2026-05-13", "deploy"},
	}
	for i, w := range wants {
		if !strings.HasPrefix(lines[i], w.date) || !strings.Contains(lines[i], w.phase) {
			t.Errorf("line %d wrong (want date=%s phase=%s): %q", i, w.date, w.phase, lines[i])
		}
	}
	if !strings.Contains(buf.String(), "PoC initialized") {
		t.Errorf("summary heading not surfaced")
	}
}

// --- journal add ----------------------------------------------------------

func TestJournalAdd_AppendsTimestamped(t *testing.T) {
	dir := makeTestPoC(t)
	var buf bytes.Buffer
	if err := runJournalAdd(&buf, dir, "pre-sales-se", "Customer accepted scope after meeting"); err != nil {
		t.Fatal(err)
	}
	notes := filepath.Join(dir, "journal", time.Now().UTC().Format("2006-01-02")+"-notes.md")
	data, err := os.ReadFile(notes)
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	for _, want := range []string{"pre-sales-se", "Customer accepted scope after meeting"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
	// Second append should preserve the first.
	if err := runJournalAdd(&buf, dir, "lab-tech", "Switch port lacp confirmed"); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(notes)
	if !strings.Contains(string(data), "Customer accepted") || !strings.Contains(string(data), "lacp confirmed") {
		t.Errorf("second append clobbered first:\n%s", string(data))
	}
}

func TestJournalAdd_OutsidePoCErrors(t *testing.T) {
	dir := t.TempDir() // no poc.yaml here
	if err := runJournalAdd(io.Discard, dir, "operator", "hi"); err == nil {
		t.Errorf("expected error when no poc.yaml")
	}
}

// --- journal report -------------------------------------------------------

func TestJournalReport_AllSectionsPresent(t *testing.T) {
	dir := makeTestPoC(t)
	writeFile(t, filepath.Join(dir, "journal", "2026-05-13-cluster.md"),
		"## lab-tech: cluster up — SUCCESS\n- log: artifacts/cluster-up.log\n")
	writeFile(t, filepath.Join(dir, "journal", "2026-05-13-provision.md"),
		"## lab-tech: provision FAILED (readiness)\n- rshim busy\n")

	p, err := poc.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	out, err := renderJournalReport(dir, p)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"# PoC report — test-poc",
		"**Customer:** Acme",
		"## Executive summary",
		"## Topology",
		"## Phase timeline",
		"## Decisions",
		"## Issues encountered",
		"## Outcome and recommendations",
		"discover", // status table
		"completed",
		"in_progress",
		"FAILED (readiness)", // issue extraction
		"Foo decided over bar", // decisions verbatim
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in report. Full report:\n%s", want, out)
		}
	}
}

func TestJournalReport_HandlesNoDecisions(t *testing.T) {
	dir := makeTestPoC(t)
	_ = os.Remove(filepath.Join(dir, "decisions.md"))
	p, _ := poc.Load(dir)
	out, err := renderJournalReport(dir, p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "_No decisions.md found._") {
		t.Errorf("expected no-decisions notice. got:\n%s", out)
	}
}

func TestJournalReport_HandlesNoJournalEntries(t *testing.T) {
	dir := makeTestPoC(t)
	p, _ := poc.Load(dir)
	out, err := renderJournalReport(dir, p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "_No journal entries recorded yet._") {
		t.Errorf("expected no-entries notice. got:\n%s", out)
	}
}

func TestJournalReport_SkipsItself(t *testing.T) {
	dir := makeTestPoC(t)
	// Pretend a previous report already exists.
	writeFile(t, filepath.Join(dir, "journal", "2026-05-13-final-report.md"),
		"# previous report — should not be inlined\n")
	writeFile(t, filepath.Join(dir, "journal", "2026-05-13-cluster.md"),
		"## cluster — SUCCESS\n")
	p, _ := poc.Load(dir)
	out, err := renderJournalReport(dir, p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "previous report") {
		t.Errorf("report inlined itself recursively:\n%s", out)
	}
	if !strings.Contains(out, "cluster — SUCCESS") {
		t.Errorf("real journal entry missing from report")
	}
}

// --- helpers --------------------------------------------------------------

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
