package examples

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mwiget/dpubnkctl/internal/poc"
)

// TestSamplesLoadAndValidate guards against schema drift between
// the examples/ files and the active poc.yaml schema in internal/poc.
//
// For each embedded sample, the test:
//
//  1. Writes the YAML body to a temp PoC repo (with stub keys/ that
//     the file references — FAR tarball, JWT, ssh key, DPU password
//     hash). The body itself is unmodified.
//  2. Loads it via poc.Load (the same strict-decode path the binary
//     uses) — fails loud on any unknown field or YAML parse error.
//  3. Runs poc.Validate(...). The sample must pass with zero
//     errors. Warnings are allowed (e.g. the internal_cidr default
//     warning is informational).
//
// If you're adding a new sample and this test fails: either fix the
// schema mismatch in your YAML, or document the intentional
// validate-time error you want operators to see (and skip the
// validation assertion for just that sample with an explicit
// allowlist).
func TestSamplesLoadAndValidate(t *testing.T) {
	samples := All()
	if len(samples) == 0 {
		t.Fatal("no samples embedded — expected at least one .yaml in examples/")
	}
	for _, s := range samples {
		t.Run(s.Name, func(t *testing.T) {
			repo := writeStubPoCRepo(t, s.Body)
			p, err := poc.Load(repo)
			if err != nil {
				t.Fatalf("poc.Load on sample %s: %v", s.Name, err)
			}
			r := poc.Validate(p, repo)
			if !r.Valid() {
				t.Errorf("sample %s does not validate — %d error(s):", s.Name, len(r.Errors))
				for _, e := range r.Errors {
					t.Errorf("  ✗ %s", e)
				}
			}
		})
	}
}

// TestSamplesHaveDescriptions makes sure every sample has a
// `# Description: ...` header — without it `dpubnkctl samples` shows
// a blank column where the description should be.
func TestSamplesHaveDescriptions(t *testing.T) {
	for _, s := range All() {
		if s.Description == "" {
			t.Errorf("sample %s has no `# Description:` comment header", s.Name)
		}
	}
}

// TestSamplesAreReachableByFind exercises Find/Names alongside All so
// future refactors don't drift between them.
func TestSamplesAreReachableByFind(t *testing.T) {
	for _, s := range All() {
		got := Find(s.Name)
		if got == nil {
			t.Errorf("Find(%q) returned nil; All() did include it", s.Name)
			continue
		}
		if got.Body != s.Body {
			t.Errorf("Find(%q) returned a different body than All()", s.Name)
		}
	}
	if Find("definitely-not-a-real-sample") != nil {
		t.Error("Find on a bogus name should return nil")
	}
}

// writeStubPoCRepo creates a temp dir containing the supplied poc.yaml
// body plus empty placeholder files for every `*_ref:` path the sample
// references. poc.Validate calls os.Stat on each referenced file, so
// missing stubs cause spurious failures.
//
// Parses refs out of the YAML rather than hard-coding a list — that
// way a new sample with a different key naming convention (e.g.
// `keys/host1`, `keys/jwt-prod`) is supported without the test
// needing an update.
func writeStubPoCRepo(t *testing.T, body string) string {
	t.Helper()
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "poc.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, rel := range refPaths(body) {
		full := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("stub"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return repo
}

// refPaths extracts the relative paths every `*_ref:` YAML key
// points at. Cheap line scan — sufficient for the validate-touch
// surface (key_ref, jwt_ref, far_key_ref, dpu_password_hash_ref).
func refPaths(body string) []string {
	var out []string
	seen := map[string]bool{}
	for _, raw := range strings.Split(body, "\n") {
		line := strings.TrimSpace(raw)
		idx := strings.Index(line, "_ref:")
		if idx == -1 {
			continue
		}
		val := strings.TrimSpace(line[idx+len("_ref:"):])
		// Strip optional inline comment + quotes.
		if hash := strings.Index(val, "#"); hash != -1 {
			val = strings.TrimSpace(val[:hash])
		}
		val = strings.Trim(val, `"'`)
		if val == "" || seen[val] {
			continue
		}
		seen[val] = true
		out = append(out, val)
	}
	return out
}
