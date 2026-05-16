package cli

import "fmt"

// requireTwoGates is the canonical implementation of dpubnkctl's
// `--yolo` + `--confirm-<thing>` destructive-action gate. Every
// subcommand that touches kubernetes, the host, or the DPU goes
// through it.
//
// Before this helper, each call site rewrote the gate inline with
// slightly different error phrasing — "refusing destructive cluster
// bring-up without --yolo", "refusing to rewrite netplan without
// --yolo", "refusing destructive deploy without --yolo". The wording
// drift made operator-facing messages inconsistent across what is
// fundamentally the same check; normalising them here keeps tooling
// (audit reports, doctor diagnoses) able to recognise the failure.
//
// `action` is the noun-phrase that goes after "refusing" — eg.
// "cluster bring-up", "cluster reset", "DPU join", "deploy",
// "teardown". `confirmFlag` is the literal flag name shown in the
// typo-guard error ("--confirm-cluster" or "--confirm-deploy").
//
// Note: `provision dpu` uses --confirm-flash with a different shape
// (must list every positional hostname) and is not covered here.
// `host network setup` adds a `--dry-run` outer skip and calls into
// this helper from the destructive branch.
func requireTwoGates(yolo bool, confirmFlag, confirmVal, pocName, action string) error {
	if !yolo {
		return fmt.Errorf("refusing destructive %s without --yolo", action)
	}
	if confirmVal != pocName {
		return fmt.Errorf("%s must equal poc.yaml.metadata.name (%q), got %q", confirmFlag, pocName, confirmVal)
	}
	return nil
}
