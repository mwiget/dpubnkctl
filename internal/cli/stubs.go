package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// notYet returns a RunE that reports a subcommand as not-yet-implemented.
// Currently used only by `cluster down` (which points operators at
// `cluster reset`); kept around because future subcommands stubbed
// before their backend ships will follow the same pattern.
func notYet(name, phase string) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		return fmt.Errorf("`dpubnkctl %s` is not implemented yet (planned for %s)", name, phase)
	}
}
