package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// notYet returns a RunE that reports a subcommand as not-yet-implemented.
// Kept around for the handful of remaining stubs (e.g. `cluster status`).
func notYet(name, phase string) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		return fmt.Errorf("`dpubnkctl %s` is not implemented yet (planned for %s)", name, phase)
	}
}
