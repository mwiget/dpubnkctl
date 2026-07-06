package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/mwiget/dpubnkctl/internal/ctlschema"
	"github.com/mwiget/dpubnkctl/internal/version"
)

// newSchemaCmd assembles the hidden `__schema` subcommand. It dumps the whole
// command tree as JSON (see internal/ctlschema) so external tooling can build
// a machine-usable catalog of dpubnkctl's CLI surface without scraping --help.
//
// It is hidden (double-underscore + Hidden) because it is a machine contract,
// not an operator-facing command. dpubnkctl gains no awareness of who consumes
// the output; the command only serialises cobra/pflag metadata it already has.
func newSchemaCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "__schema",
		Short:  "Emit the CLI command/flag tree as JSON (machine introspection)",
		Hidden: true,
		Args:   cobra.NoArgs,
		// No preflight: this reads nothing but in-memory metadata.
		RunE: func(cmd *cobra.Command, _ []string) error {
			schema := ctlschema.Walk(cmd.Root(), ctlschema.Meta{
				Ctl:        cmd.Root().Name(),
				CtlVersion: version.Version,
				BNKVersion: version.BNKVersion,
			})
			data, err := json.MarshalIndent(schema, "", "  ")
			if err != nil {
				return fmt.Errorf("marshal schema: %w", err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), string(data))
			return nil
		},
	}
}
