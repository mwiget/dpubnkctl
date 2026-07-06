package ctlschema

import (
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// buildTree returns a small root mirroring dpubnkctl's shape: a runnable
// read-only leaf, a grouping parent with a gated destructive child, plus the
// cobra-injected completion command that Walk must skip.
func buildTree() *cobra.Command {
	root := &cobra.Command{Use: "dpubnkctl"}

	version := &cobra.Command{Use: "version", Short: "print version", Run: func(*cobra.Command, []string) {}}

	destroy := &cobra.Command{Use: "destroy", Short: "tear down (parent)"}
	dpus := &cobra.Command{
		Use:   "dpus",
		Short: "reset every DPU",
		Run:   func(*cobra.Command, []string) {},
	}
	dpus.Flags().Bool("yolo", false, "acknowledge destructive")
	dpus.Flags().String("confirm-cluster", "", "typo guard")
	dpus.Flags().Duration("timeout", 5*time.Minute, "per-dpu wall clock")
	_ = dpus.MarkFlagRequired("confirm-cluster")
	destroy.AddCommand(dpus)

	hidden := &cobra.Command{Use: "__schema", Short: "introspect", Hidden: true, Run: func(*cobra.Command, []string) {}}

	root.AddCommand(version, destroy, hidden)
	root.InitDefaultCompletionCmd() // cobra scaffolding Walk must ignore
	return root
}

func find(s CtlSchema, path string) *Command {
	for i := range s.Commands {
		if s.Commands[i].Path == path {
			return &s.Commands[i]
		}
	}
	return nil
}

func TestWalk_pathsAndMeta(t *testing.T) {
	s := Walk(buildTree(), Meta{Ctl: "dpubnkctl", CtlVersion: "1.2.3", BNKVersion: "2.3.1"})

	if s.Ctl != "dpubnkctl" || s.CtlVersion != "1.2.3" || s.BNKVersion != "2.3.1" {
		t.Fatalf("meta not carried through: %+v", s)
	}
	if s.SchemaVersion != SchemaVersion {
		t.Fatalf("schemaVersion = %d, want %d", s.SchemaVersion, SchemaVersion)
	}

	// Paths are root-relative; nested command keeps the space-joined path.
	if find(s, "version") == nil {
		t.Error("missing read-only leaf 'version'")
	}
	if find(s, "destroy") == nil {
		t.Error("missing grouping parent 'destroy'")
	}
	if find(s, "destroy dpus") == nil {
		t.Error("missing nested 'destroy dpus'")
	}

	// Framework commands are skipped.
	if find(s, "completion") != nil {
		t.Error("completion command should be skipped")
	}
	if find(s, "help") != nil {
		t.Error("help command should be skipped")
	}

	// Commands are sorted by path for stable output.
	for i := 1; i < len(s.Commands); i++ {
		if s.Commands[i-1].Path > s.Commands[i].Path {
			t.Fatalf("commands not sorted: %q before %q", s.Commands[i-1].Path, s.Commands[i].Path)
		}
	}
}

func TestWalk_runnableAndHidden(t *testing.T) {
	s := Walk(buildTree(), Meta{Ctl: "dpubnkctl"})

	if parent := find(s, "destroy"); parent == nil || parent.Runnable {
		t.Errorf("grouping parent 'destroy' should be non-runnable, got %+v", parent)
	}
	if leaf := find(s, "destroy dpus"); leaf == nil || !leaf.Runnable {
		t.Errorf("'destroy dpus' should be runnable, got %+v", leaf)
	}
	if h := find(s, "__schema"); h == nil || !h.Hidden {
		t.Errorf("'__schema' should be present and hidden, got %+v", h)
	}
}

func TestWalk_flags(t *testing.T) {
	s := Walk(buildTree(), Meta{Ctl: "dpubnkctl"})
	leaf := find(s, "destroy dpus")
	if leaf == nil {
		t.Fatal("missing 'destroy dpus'")
	}

	byName := map[string]Flag{}
	for _, f := range leaf.Flags {
		byName[f.Name] = f
	}

	yolo, ok := byName["yolo"]
	if !ok || yolo.Type != "bool" || yolo.Default != "false" {
		t.Errorf("yolo flag wrong: %+v", yolo)
	}
	confirm, ok := byName["confirm-cluster"]
	if !ok || confirm.Type != "string" || !confirm.Required {
		t.Errorf("confirm-cluster flag wrong (want required string): %+v", confirm)
	}
	timeout, ok := byName["timeout"]
	if !ok || timeout.Type != "duration" {
		t.Errorf("timeout flag wrong: %+v", timeout)
	}

	// Flags are sorted by name.
	for i := 1; i < len(leaf.Flags); i++ {
		if leaf.Flags[i-1].Name > leaf.Flags[i].Name {
			t.Fatalf("flags not sorted: %q before %q", leaf.Flags[i-1].Name, leaf.Flags[i].Name)
		}
	}
}
