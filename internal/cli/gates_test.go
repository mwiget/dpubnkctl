package cli

import (
	"strings"
	"testing"
)

// TestRequireTwoGates pins down the canonical destructive-action gate
// used by 11 subcommands. Each input dimension (yolo, confirm match)
// is exercised independently so a future tweak to either error string
// fails loud.
func TestRequireTwoGates(t *testing.T) {
	cases := []struct {
		name         string
		yolo         bool
		confirmFlag  string
		confirmVal   string
		pocName      string
		action       string
		wantErr      bool
		wantContains string
	}{
		{
			name:         "missing_yolo",
			yolo:         false,
			confirmFlag:  "--confirm-cluster",
			confirmVal:   "homelab",
			pocName:      "homelab",
			action:       "cluster bring-up",
			wantErr:      true,
			wantContains: "refusing destructive cluster bring-up without --yolo",
		},
		{
			name:         "confirm_mismatch",
			yolo:         true,
			confirmFlag:  "--confirm-cluster",
			confirmVal:   "lake1",
			pocName:      "homelab",
			action:       "cluster bring-up",
			wantErr:      true,
			wantContains: `--confirm-cluster must equal poc.yaml.metadata.name ("homelab"), got "lake1"`,
		},
		{
			name:         "confirm_empty",
			yolo:         true,
			confirmFlag:  "--confirm-deploy",
			confirmVal:   "",
			pocName:      "homelab",
			action:       "deploy",
			wantErr:      true,
			wantContains: "--confirm-deploy must equal",
		},
		{
			name:        "both_ok",
			yolo:        true,
			confirmFlag: "--confirm-cluster",
			confirmVal:  "homelab",
			pocName:     "homelab",
			action:      "teardown",
			wantErr:     false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := requireTwoGates(tc.yolo, tc.confirmFlag, tc.confirmVal, tc.pocName, tc.action)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected nil, got: %v", err)
			}
			if tc.wantContains != "" && !strings.Contains(err.Error(), tc.wantContains) {
				t.Errorf("error %q does not contain %q", err, tc.wantContains)
			}
		})
	}
}
