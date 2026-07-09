package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mwiget/dpubnkctl/internal/poc"
)

func readClusterJournal(t *testing.T, repo string) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(repo, "journal", "*-cluster.md"))
	if err != nil || len(matches) == 0 {
		t.Fatalf("no cluster journal written (glob err=%v, matches=%v)", err, matches)
	}
	b, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func oneJoinJob() []dpuJob {
	return []dpuJob{{
		host: &poc.Host{Name: "host1"},
		dpu:  &poc.DPU{PCI: "0000:03:00.0", Hostname: "host1-bf3", TmfifoIP: "192.168.100.2/30"},
	}}
}

func TestAppendJoinJournal_Success(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "journal"), 0o755); err != nil {
		t.Fatal(err)
	}
	appendJoinJournal(repo, "test-poc", oneJoinJob(), nil)
	body := readClusterJournal(t, repo)
	if !strings.Contains(body, "labeled app=f5-tmm, tainted dpu=true:NoSchedule") {
		t.Errorf("success journal should record the label/taint:\n%s", body)
	}
	for _, bad := range []string{"FAILED", "NOT BNK-ready", "NOT labeled/tainted"} {
		if strings.Contains(body, bad) {
			t.Errorf("success journal must not contain %q:\n%s", bad, body)
		}
	}
}

// TestAppendJoinJournal_LabelTaintFailure is the core Fix 4 guard: when
// label/taint failed, the journal must NOT falsely claim the node was
// labeled/tainted, and must point at the required retry.
func TestAppendJoinJournal_LabelTaintFailure(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "journal"), 0o755); err != nil {
		t.Fatal(err)
	}
	appendJoinJournal(repo, "test-poc", oneJoinJob(), errors.New("kubectl label: connection refused"))
	body := readClusterJournal(t, repo)
	if strings.Contains(body, "labeled app=f5-tmm, tainted dpu=true:NoSchedule") {
		t.Errorf("failure journal must NOT claim label/taint success:\n%s", body)
	}
	for _, want := range []string{
		"label/taint FAILED",
		"joined but NOT labeled/tainted",
		"connection refused",
		"NOT BNK-ready",
		"cluster join-dpus",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("failure journal missing %q:\n%s", want, body)
		}
	}
}
