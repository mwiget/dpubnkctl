package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// openJournal creates (or appends to) journal/<date>-<phase>.md and
// writes the canonical "## lab-tech: <header>" + "- Time: <RFC3339>"
// preamble. Returns the open file so phase-specific callers can append
// their own field lines. Caller MUST Close.
//
// File mode 0o600: journal entries carry unredacted kubectl/helm output
// that can include JWT bodies, FAR credentials, kubeadm join tokens —
// other local users on the jumphost shouldn't read them.
//
// Used by appendClusterJournal, appendJoinJournal, appendDeployJournal,
// appendDestroyJournal, and appendFlashJournal. Before this helper each
// of those re-implemented the open + preamble + close shape with
// minor formatting drift. discover's journal uses a different time
// source (the discover Result's timestamp) and stays out of scope.
func openJournal(repo, phase, header string) (*os.File, error) {
	date := time.Now().UTC().Format("2006-01-02")
	path := filepath.Join(repo, "journal", date+"-"+phase+".md")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(f, "## lab-tech: %s\n", header)
	fmt.Fprintf(f, "- Time: %s\n", time.Now().UTC().Format(time.RFC3339))
	return f, nil
}
