package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/mwiget/dpubnkctl/internal/poc"
)

func newValidateCmd() *cobra.Command {
	var (
		pocDir string
		strict bool
	)
	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Statically validate poc.yaml (and referenced files) before any phase",
		Long: `Walk poc.yaml and report every field that's missing, inconsistent, or
still at template defaults. Confirms the customer-supplied keys (FAR,
JWT, SSH, password hash) referenced from poc.yaml exist on disk.

Errors mean some phase command will fail-loud anyway:
  - missing host/DPU fields needed by bf.conf render or kubespray plan
  - non-LAG DPU with VLANs lacking uplink p0|p1
  - VLAN role/tag/IP/port-name invalid
  - referenced FAR / JWT / SSH key file missing

Warnings are values that should be confirmed but don't block:
  - network.internal_cidr still at the template default
  - network.cluster_apiserver_address / node_ip_role empty
  - bnk.external_selfip / internal_selfip empty
  - 2 control planes (HA-unsafe)

Exit code 0 on no errors. --strict treats warnings as failures.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runValidate(cmd.OutOrStdout(), pocDir, strict)
		},
	}
	cmd.Flags().StringVar(&pocDir, "poc", "", "PoC repo path (default: current directory)")
	cmd.Flags().BoolVar(&strict, "strict", false, "Treat warnings as failures (non-zero exit)")
	return cmd
}

func runValidate(out io.Writer, pocDir string, strict bool) error {
	repo, err := resolvePoCDir(pocDir)
	if err != nil {
		return err
	}
	p, err := poc.Load(repo)
	if err != nil {
		return fmt.Errorf("not a PoC repo (%s): %w", repo, err)
	}
	r := poc.Validate(p, repo)
	printValidation(out, r)
	if len(r.Errors) > 0 {
		return fmt.Errorf("validate: %d error(s)", len(r.Errors))
	}
	if strict && len(r.Warnings) > 0 {
		return fmt.Errorf("validate: %d warning(s) with --strict", len(r.Warnings))
	}
	return nil
}

// printValidation writes errors and warnings to out in a stable format.
// Used by `dpubnkctl validate` and by the precheck in `provision dpus`.
func printValidation(out io.Writer, r poc.ValidationResult) {
	if len(r.Errors) == 0 && len(r.Warnings) == 0 {
		fmt.Fprintln(out, "poc.yaml validates clean.")
		return
	}
	if len(r.Errors) > 0 {
		fmt.Fprintf(out, "Errors (%d):\n", len(r.Errors))
		for _, e := range r.Errors {
			fmt.Fprintf(out, "  ✗ %s\n", e)
		}
	}
	if len(r.Warnings) > 0 {
		fmt.Fprintf(out, "Warnings (%d):\n", len(r.Warnings))
		for _, w := range r.Warnings {
			fmt.Fprintf(out, "  ! %s\n", w)
		}
	}
}
