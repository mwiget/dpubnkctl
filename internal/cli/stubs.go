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

func newProvisionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "provision",
		Short: "Flash and configure DPUs (Phase 2, not yet implemented)",
		RunE:  notYet("provision", "Phase 2"),
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "dpu <serial-or-bmc-ip>",
		Short: "Flash a single DPU with the pinned BFB image (LAG or non-LAG per poc.yaml)",
		Args:  cobra.ExactArgs(1),
		RunE:  notYet("provision dpu", "Phase 2"),
	})
	return cmd
}

func newClusterCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cluster",
		Short: "Bring up the Kubernetes cluster (Phase 3, not yet implemented)",
		RunE:  notYet("cluster", "Phase 3"),
	}
	cmd.AddCommand(
		&cobra.Command{Use: "up", Short: "kubespray (>=3 hosts) or kubeadm (1-2 hosts)", RunE: notYet("cluster up", "Phase 3")},
		&cobra.Command{Use: "status", Short: "Show cluster + node readiness", RunE: notYet("cluster status", "Phase 3")},
		&cobra.Command{Use: "down", Short: "Tear down the cluster", RunE: notYet("cluster down", "Phase 3")},
	)
	return cmd
}

func newDeployCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "deploy",
		Short: "Install BNK platform (FLO, CNEInstance, VLANs, GatewayClass) (Phase 4)",
		RunE:  notYet("deploy", "Phase 4"),
	}
}

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
