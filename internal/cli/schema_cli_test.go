package cli

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/mwiget/dpubnkctl/internal/ctlschema"
)

// TestSchemaCommand exercises the real command tree end-to-end: `dpubnkctl
// __schema` must emit a parseable document that includes the actual commands
// and their gate flags, so bnk-forge's catalog generator has a stable contract
// to build against.
func TestSchemaCommand(t *testing.T) {
	root := NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"__schema"})

	if err := root.Execute(); err != nil {
		t.Fatalf("__schema failed: %v\noutput:\n%s", err, out.String())
	}

	var schema ctlschema.CtlSchema
	if err := json.Unmarshal(out.Bytes(), &schema); err != nil {
		t.Fatalf("output is not valid schema JSON: %v\n%s", err, out.String())
	}

	if schema.Ctl != "dpubnkctl" {
		t.Errorf("ctl = %q, want dpubnkctl", schema.Ctl)
	}
	if schema.SchemaVersion != ctlschema.SchemaVersion {
		t.Errorf("schemaVersion = %d, want %d", schema.SchemaVersion, ctlschema.SchemaVersion)
	}
	if schema.BNKVersion == "" {
		t.Error("bnkVersion should be stamped from internal/version")
	}

	byPath := map[string]ctlschema.Command{}
	for _, c := range schema.Commands {
		byPath[c.Path] = c
	}

	// A gated destructive leaf carries its Launch-prerequisite flags.
	dpus, ok := byPath["destroy dpus"]
	if !ok {
		t.Fatalf("missing 'destroy dpus'; got paths: %v", keys(byPath))
	}
	if !dpus.Runnable {
		t.Error("'destroy dpus' should be runnable")
	}
	assertHasFlag(t, dpus, "yolo", "bool")
	assertHasFlag(t, dpus, "confirm-cluster", "string")

	// A read-only leaf is present.
	if _, ok := byPath["version"]; !ok {
		t.Error("missing read-only 'version' command")
	}

	// The __schema command itself is present but hidden.
	if s, ok := byPath["__schema"]; !ok || !s.Hidden {
		t.Errorf("'__schema' should be present and hidden, got %+v (present=%v)", s, ok)
	}

	// Framework scaffolding is excluded.
	if _, ok := byPath["completion"]; ok {
		t.Error("'completion' should be excluded")
	}
	if _, ok := byPath["help"]; ok {
		t.Error("'help' should be excluded")
	}
}

// TestSchemaHiddenFromHelp guards that adding __schema did not leak it into the
// operator-facing help output.
func TestSchemaHiddenFromHelp(t *testing.T) {
	root := NewRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("--help failed: %v", err)
	}
	if bytes.Contains(out.Bytes(), []byte("__schema")) {
		t.Errorf("__schema should be hidden from --help, but appeared:\n%s", out.String())
	}
}

func assertHasFlag(t *testing.T, c ctlschema.Command, name, typ string) {
	t.Helper()
	for _, f := range c.Flags {
		if f.Name == name {
			if f.Type != typ {
				t.Errorf("flag %q type = %q, want %q", name, f.Type, typ)
			}
			return
		}
	}
	t.Errorf("command %q missing flag %q", c.Path, name)
}

func keys(m map[string]ctlschema.Command) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
