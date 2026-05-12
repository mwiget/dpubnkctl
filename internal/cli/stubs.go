package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// Phase 0 stubs. Real implementations land in later phases:
//   discover  -> Phase 1
//   provision -> Phase 2
//   cluster   -> Phase 3
//   deploy    -> Phase 4
//   destroy   -> Phase 4 (paired with deploy)
//   journal   -> Phase 5


func newDestroyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "destroy",
		Short: "Tear down everything described in poc.yaml (Phase 4)",
		RunE:  notYet("destroy", "Phase 4"),
	}
}

func newJournalCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "journal",
		Short: "Manage the PoC journal (Phase 5, not yet implemented)",
		RunE:  notYet("journal", "Phase 5"),
	}
	cmd.AddCommand(
		&cobra.Command{Use: "add <message>", Short: "Append a journal entry", RunE: notYet("journal add", "Phase 5")},
		&cobra.Command{Use: "report", Short: "Render the final lessons-learned report", RunE: notYet("journal report", "Phase 5")},
	)
	return cmd
}

func notYet(name, phase string) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		return fmt.Errorf("`dpubnkctl %s` is not implemented yet (planned for %s)", name, phase)
	}
}
