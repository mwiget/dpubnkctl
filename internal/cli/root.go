package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mwiget/dpubnkctl/internal/cluster"
	"github.com/mwiget/dpubnkctl/internal/version"
)

// NewRootCmd assembles the cobra command tree.
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "dpubnkctl",
		Short: "Deploy F5 BIG-IP Next for Kubernetes (BNK) on bare-metal hosts with NVIDIA BlueField DPUs",
		Long: `dpubnkctl provisions a BNK ` + version.BNKVersion + ` deployment end-to-end:

  discover  -> probe hosts/DPUs over SSH+Redfish, build inventory
  provision -> flash DPUs (BFB ` + version.DOCAVersion + `), set NIC mode, configure networking
  cluster   -> bring up the Kubernetes control plane and join DPU workers
  deploy    -> install BNK platform (FLO, CNEInstance, VLANs, GatewayClass)

Each PoC lives in its own local git repo (see "init"). The repo holds full
declarative state so you can tear down and redeploy without re-prompting.

Two operating modes:
  - Human:    run subcommands directly with --interactive prompts
  - Agentic:  point your favorite agentic CLI at the PoC repo's AGENTS.md
              (see "dpubnkctl agent --help")

Run "dpubnkctl doctor" once after install to verify host-side requirements
(container runtime — docker or podman, git, mgmt-network reachability)
before driving a PoC.`,
		SilenceUsage:      true,
		PersistentPreRunE: rootPreflight,
	}

	root.AddCommand(
		newInitCmd(),
		newDiscoverCmd(),
		newProvisionCmd(),
		newHostCmd(),
		newClusterCmd(),
		newDeployCmd(),
		newDestroyCmd(),
		newJournalCmd(),
		newAgentCmd(),
		newDoctorCmd(),
		newValidateCmd(),
		newTopologiesCmd(),
		newVersionCmd(),
	)

	return root
}

// rootPreflight runs once before every subcommand. For the runtime-
// dependent subtrees (cluster up/reset/join-dpus, deploy *, destroy *) it
// verifies a container runtime — docker or podman — is reachable so the
// operator hears about it up front rather than 30 minutes into a kubespray
// run. Everything else short-circuits.
//
// Add new runtime-dependent commands to runtimeRequiredCommands below.
func rootPreflight(cmd *cobra.Command, args []string) error {
	if !commandNeedsRuntime(cmd.CommandPath()) {
		return nil
	}
	if _, err := cluster.CheckContainerRuntime(cmd.Context()); err != nil {
		return fmt.Errorf("%w\n\nRun `dpubnkctl doctor` to verify all host-side requirements", err)
	}
	return nil
}

// runtimeRequiredCommands is the explicit allowlist of command paths that
// shell out to a container runtime (kubespray or alpine/k8s containers).
// Listed by full `cmd.CommandPath()` (matches via HasPrefix so e.g.
// `destroy` covers `destroy bnk` and `destroy dpus`).
var runtimeRequiredCommands = []string{
	"dpubnkctl cluster up",
	"dpubnkctl cluster reset",
	"dpubnkctl cluster join-dpus",
	"dpubnkctl deploy",
	"dpubnkctl destroy",
}

func commandNeedsRuntime(path string) bool {
	for _, p := range runtimeRequiredCommands {
		if path == p || strings.HasPrefix(path, p+" ") {
			return true
		}
	}
	return false
}
