package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestClearE2EPhases covers the destroy → state-cleanup hook added
// for issue #3: after a successful destroy, the e2e state file must
// drop the phases whose work just got unwound so the next
// `dpubnkctl e2e --yolo` doesn't skip them as "already done".
func TestClearE2EPhases(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "artifacts"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Seed a state file matching what a finished e2e leaves behind.
	now := time.Now().UTC()
	seed := e2eState{Phases: map[string]e2ePhaseState{
		"provision":         {Status: "ok", CompletedAt: now, Duration: "12m31s"},
		"host-network":      {Status: "ok", CompletedAt: now, Duration: "5s"},
		"cluster-up":        {Status: "ok", CompletedAt: now, Duration: "4m31s"},
		"cluster-join-dpus": {Status: "ok", CompletedAt: now, Duration: "1m3s"},
		"deploy-network":    {Status: "ok", CompletedAt: now, Duration: "1m24s"},
		"deploy-flo":        {Status: "ok", CompletedAt: now, Duration: "56s"},
		"deploy-cne":        {Status: "ok", CompletedAt: now, Duration: "19m20s"},
	}}
	if err := saveE2EState(dir, seed); err != nil {
		t.Fatalf("save seed: %v", err)
	}

	// Full destroy clears every cluster/deploy-dependent phase plus
	// provision (since dpus get reflashed). validate is not stored.
	full := []string{
		"provision", "host-network", "cluster-up", "cluster-join-dpus",
		"deploy-network", "deploy-flo", "deploy-cne",
	}
	if err := clearE2EPhases(dir, full...); err != nil {
		t.Fatalf("clear: %v", err)
	}
	got := loadE2EState(dir)
	if len(got.Phases) != 0 {
		t.Fatalf("expected all phases cleared, got %v", got.Phases)
	}
}

// TestClearE2EPhasesPartial — `destroy bnk` (skipDPUs+skipCluster set)
// only invalidates the deploy-* layer; provision/host-network/
// cluster-up/cluster-join-dpus must survive.
func TestClearE2EPhasesPartial(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "artifacts"), 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	seed := e2eState{Phases: map[string]e2ePhaseState{
		"provision":         {Status: "ok", CompletedAt: now},
		"host-network":      {Status: "ok", CompletedAt: now},
		"cluster-up":        {Status: "ok", CompletedAt: now},
		"cluster-join-dpus": {Status: "ok", CompletedAt: now},
		"deploy-network":    {Status: "ok", CompletedAt: now},
		"deploy-flo":        {Status: "ok", CompletedAt: now},
		"deploy-cne":        {Status: "ok", CompletedAt: now},
	}}
	if err := saveE2EState(dir, seed); err != nil {
		t.Fatal(err)
	}

	if err := clearE2EPhases(dir, "deploy-network", "deploy-flo", "deploy-cne"); err != nil {
		t.Fatal(err)
	}
	got := loadE2EState(dir)
	for _, ph := range []string{"deploy-network", "deploy-flo", "deploy-cne"} {
		if _, ok := got.Phases[ph]; ok {
			t.Errorf("%s should have been cleared", ph)
		}
	}
	for _, ph := range []string{"provision", "host-network", "cluster-up", "cluster-join-dpus"} {
		if _, ok := got.Phases[ph]; !ok {
			t.Errorf("%s should have survived a bnk-only destroy", ph)
		}
	}
}

// TestClearE2EPhasesMissingFile — a missing state file is a noop, not
// an error. destroy must succeed even on a never-run PoC.
func TestClearE2EPhasesMissingFile(t *testing.T) {
	dir := t.TempDir()
	if err := clearE2EPhases(dir, "provision", "cluster-up"); err != nil {
		t.Fatalf("expected noop, got error: %v", err)
	}
}
