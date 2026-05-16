// Package examples ships curated, working poc.yaml templates inside
// the dpubnkctl binary. Each YAML file in this directory is fully
// validatable by `dpubnkctl validate` once the operator edits the
// CUSTOMIZE-marked fields.
//
// The CLI exposes this content as `dpubnkctl samples [list|show|
// extract]`. Operators can also browse the source on GitHub:
//
//	https://github.com/mwiget/dpubnkctl/tree/main/examples
//
// Adding a new sample:
//
//  1. Drop the .yaml file next to this embed.go.
//  2. Make sure the first non-blank comment line is
//     `# Description: <one-line summary>` — that's what
//     `dpubnkctl samples` prints in the listing.
//  3. Mark every field the operator must change with a trailing
//     `# CUSTOMIZE: <hint>` comment.
//  4. Confirm the sample passes `go test ./examples` (the embedded
//     -samples-validate test loads each file through poc.Load + poc.Validate
//     so schema drift is caught at CI time, not in the field).
package examples

import (
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

//go:embed *.yaml
var FS embed.FS

// Sample is one named poc.yaml template along with its parsed
// description.
type Sample struct {
	Name        string
	Description string
	Body        string
}

// All returns every sample sorted by Name.
func All() []Sample {
	entries, err := fs.ReadDir(FS, ".")
	if err != nil {
		return nil
	}
	var out []Sample
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		body, err := fs.ReadFile(FS, e.Name())
		if err != nil {
			continue
		}
		out = append(out, Sample{
			Name:        strings.TrimSuffix(e.Name(), ".yaml"),
			Description: parseDescription(string(body)),
			Body:        string(body),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Find returns the named sample, or nil if not present.
func Find(name string) *Sample {
	for _, s := range All() {
		if s.Name == name {
			return &s
		}
	}
	return nil
}

// Names returns sample names sorted alphabetically. Convenient for
// cobra completion and "available samples: ..." error messages.
func Names() []string {
	samples := All()
	names := make([]string, 0, len(samples))
	for _, s := range samples {
		names = append(names, s.Name)
	}
	return names
}

// ErrNotFound builds the operator-facing error returned when a CLI
// argument names a sample that doesn't exist. Lists every available
// name so the operator doesn't have to re-run `samples list`.
func ErrNotFound(want string) error {
	names := Names()
	if len(names) == 0 {
		return fmt.Errorf("no samples embedded in this build")
	}
	return fmt.Errorf("no sample named %q — available: %s", want, strings.Join(names, ", "))
}

// parseDescription scans body until the first non-comment, non-blank
// line and returns the value of the first `# Description: ...`
// comment encountered. Returns "" if none found.
func parseDescription(body string) string {
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if !strings.HasPrefix(trimmed, "#") {
			return ""
		}
		rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "#"))
		const marker = "description:"
		if len(rest) > len(marker) && strings.EqualFold(rest[:len(marker)], marker) {
			return strings.TrimSpace(rest[len(marker):])
		}
	}
	return ""
}
