package cli

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/mwiget/dpubnkctl/internal/ssh"
)

// fwRunner returns queued results in order (one per Run call), repeating the
// last once exhausted, and records how many times Run was invoked. It
// satisfies discover.Runner.
type fwRunner struct {
	results []ssh.Result
	calls   int
}

func (f *fwRunner) Run(_ context.Context, _ string) ssh.Result {
	i := f.calls
	f.calls++
	if i < len(f.results) {
		return f.results[i]
	}
	return f.results[len(f.results)-1]
}

// TestFWResetWithRetry pins the retry policy: only the transient sync-refusal
// is retried; every other failure fails fast without hammering the device.
func TestFWResetWithRetry(t *testing.T) {
	orig := fwResetRetryGap
	fwResetRetryGap = time.Millisecond
	t.Cleanup(func() { fwResetRetryGap = orig })

	transient := ssh.Result{ExitCode: 1, Stderr: "-E- " + fwResetTransientRefusal + " in the current state"}
	permanent := ssh.Result{ExitCode: 1, Stderr: "-E- Failed to open device: 0000:99:00.0"}
	ok := ssh.Result{ExitCode: 0}

	t.Run("transient_then_success", func(t *testing.T) {
		r := &fwRunner{results: []ssh.Result{transient, transient, ok}}
		if err := fwResetWithRetry(context.Background(), io.Discard, r, "h", "0000:0d:00.0"); err != nil {
			t.Fatalf("expected success after transient retries, got %v", err)
		}
		if r.calls != 3 {
			t.Fatalf("expected 3 calls (2 transient + success), got %d", r.calls)
		}
	})

	t.Run("permanent_fails_fast", func(t *testing.T) {
		r := &fwRunner{results: []ssh.Result{permanent}}
		err := fwResetWithRetry(context.Background(), io.Discard, r, "h", "0000:0d:00.0")
		if err == nil {
			t.Fatal("expected error on permanent failure")
		}
		if r.calls != 1 {
			t.Fatalf("permanent failure must fail fast (1 call), got %d", r.calls)
		}
		if !strings.Contains(err.Error(), "Failed to open device") {
			t.Fatalf("error must surface the real mlxfwreset message, got %v", err)
		}
	})

	t.Run("transient_exhausted", func(t *testing.T) {
		r := &fwRunner{results: []ssh.Result{transient}}
		err := fwResetWithRetry(context.Background(), io.Discard, r, "h", "0000:0d:00.0")
		if err == nil {
			t.Fatal("expected error after exhausting attempts")
		}
		if r.calls != 4 {
			t.Fatalf("expected 4 attempts, got %d", r.calls)
		}
		if !strings.Contains(err.Error(), "after 4 attempts") {
			t.Fatalf("expected exhaustion message, got %v", err)
		}
	})
}
