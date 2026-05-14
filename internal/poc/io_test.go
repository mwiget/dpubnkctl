package poc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writePoCYAML drops a poc.yaml file in a fresh tempdir and returns the
// dir path. Used by Load tests to exercise the strict-decode path
// against shaped fixtures rather than ones built in Go.
func writePoCYAML(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLoad_StrictRejectsUnknownField(t *testing.T) {
	// The homelab SE wrote `role`/`tag` on network.vlans[] entries
	// (matching the prose checklist + per-DPU shape). yaml.v3 default
	// silently dropped the values — load succeeded with empty VLAN
	// structs, only surfacing two phases later. Strict decode must
	// fail at Load time with the corrective hint.
	yaml := `apiVersion: dpubnkctl.f5.com/v1alpha1
kind: PoC
metadata:
  name: bad
network:
  vlans:
    - role: external
      tag: 40
      subnet: 10.10.40.0/24
`
	dir := writePoCYAML(t, yaml)
	_, err := Load(dir)
	if err == nil {
		t.Fatal("Load must reject network.vlans with role/tag keys")
	}
	if !strings.Contains(err.Error(), "role") {
		t.Errorf("expected error to name the offending key 'role', got: %v", err)
	}
	if !strings.Contains(err.Error(), "name, id, subnet") {
		t.Errorf("expected corrective hint pointing at name/id/subnet shape, got: %v", err)
	}
}

func TestLoad_StrictRejectsArbitraryTypo(t *testing.T) {
	// Catch any unknown field anywhere, not just the documented one.
	// A `widget:` at the top level should fail without a hint — the
	// generic strict-decode error is fine; goal is "don't silently drop".
	yaml := `apiVersion: dpubnkctl.f5.com/v1alpha1
kind: PoC
widget: 42
`
	dir := writePoCYAML(t, yaml)
	if _, err := Load(dir); err == nil {
		t.Fatal("Load must reject unknown top-level field `widget`")
	}
}

func TestLoad_AcceptsValidShape(t *testing.T) {
	// Smoke test: strict mode must NOT break the standard shape that
	// New() / Save() emit. Minimal valid PoC.
	yaml := `apiVersion: dpubnkctl.f5.com/v1alpha1
kind: PoC
metadata:
  name: ok
network:
  vlans:
    - name: external
      id: 40
      subnet: 10.10.40.0/24
`
	dir := writePoCYAML(t, yaml)
	p, err := Load(dir)
	if err != nil {
		t.Fatalf("clean shape must load: %v", err)
	}
	if p.Metadata.Name != "ok" || len(p.Network.VLANs) != 1 {
		t.Errorf("unexpected parsed PoC: %+v", p)
	}
}
