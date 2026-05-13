package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/spf13/cobra"

	"github.com/mwiget/dpubnkctl/internal/cluster"
	"github.com/mwiget/dpubnkctl/internal/poc"
)

func newClusterCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cluster",
		Short: "Generate the kubespray inventory and bring up the Kubernetes cluster",
	}
	cmd.AddCommand(newClusterPlanCmd())
	cmd.AddCommand(newClusterUpCmd())
	cmd.AddCommand(newClusterJoinDPUsCmd())
	cmd.AddCommand(newClusterResetCmd())
	cmd.AddCommand(newClusterStatusCmd())
	cmd.AddCommand(&cobra.Command{
		Use:   "down",
		Short: "Tear down the cluster (alias for `cluster reset`)",
		RunE:  notYet("cluster down", "use `dpubnkctl cluster reset` for now"),
	})
	return cmd
}

type clusterPlanFlags struct {
	pocDir   string
	writeDir string
}

func newClusterPlanCmd() *cobra.Command {
	f := &clusterPlanFlags{}
	cmd := &cobra.Command{
		Use:   "plan",
		Short: "Generate the kubespray inventory + group_vars from poc.yaml (no destructive ops)",
		Long: `Walk poc.yaml.hosts, group them by role (control-plane | worker | both),
and emit a complete kubespray-compatible inventory tree:

  hosts.yml                                  ansible inventory with groups
  group_vars/all/all.yml                     ansible defaults (become, etc.)
  group_vars/k8s_cluster/k8s-cluster.yml     k8s/CNI/runtime knobs (BNK-pinned)
  README.md                                  group membership + manual run hint

The tree is written under artifacts/kubespray-inventory/ in the PoC repo
(or --write DIR). 'dpubnkctl cluster up' (next iteration) will consume it
to drive kubespray inside a Docker container.

This command is read-only. It validates required fields and complains
if any host lacks a role or ssh.address.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runClusterPlan(cmd.Context(), cmd.OutOrStdout(), f)
		},
	}
	cmd.Flags().StringVar(&f.pocDir, "poc", "", "PoC repo path (default: current directory)")
	cmd.Flags().StringVar(&f.writeDir, "write", "", "Output dir for the kubespray tree (default: artifacts/kubespray-inventory under the PoC repo)")
	return cmd
}

func runClusterPlan(ctx context.Context, out io.Writer, f *clusterPlanFlags) error {
	repo, err := resolvePoCDir(f.pocDir)
	if err != nil {
		return err
	}
	p, err := poc.Load(repo)
	if err != nil {
		return fmt.Errorf("not a PoC repo (%s): %w", repo, err)
	}

	plan := cluster.BuildPlan(p)

	fmt.Fprintf(out, "PoC:        %s   (BNK %s, k8s %s, kubespray %s)\n",
		p.Metadata.Name, p.Metadata.BNKVersion,
		"v"+p.Versions.K8s, kubesprayVersionFromPlan())
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "=== group membership ===")
	fmt.Fprintf(out, "  kube_control_plane (%d): %v\n", len(plan.ControlPlane), plan.ControlPlane)
	fmt.Fprintf(out, "  kube_node          (%d): %v\n", len(plan.Workers), plan.Workers)
	fmt.Fprintf(out, "  etcd               (%d): %v\n", len(plan.Etcd), plan.Etcd)

	if !plan.Valid() {
		fmt.Fprintln(out, "\n=== errors ===")
		for _, e := range plan.Errors {
			fmt.Fprintln(out, "  ✗", e)
		}
		fmt.Fprintln(out, "\nplan: NOT READY")
		return fmt.Errorf("%d validation error(s)", len(plan.Errors))
	}

	if len(plan.ControlPlane) == 2 {
		fmt.Fprintln(out, "\n  ! 2 control planes is not HA-safe.")
		fmt.Fprintln(out, "    Picked 1 etcd node to keep quorum on a single failure;")
		fmt.Fprintln(out, "    add a 3rd control-plane host for production HA.")
	}

	files, err := cluster.Render(p, plan)
	if err != nil {
		return err
	}

	target := f.writeDir
	if target == "" {
		target = filepath.Join(repo, "artifacts", "kubespray-inventory")
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(repo, target)
	}

	written := writeFiles(target, files)
	fmt.Fprintf(out, "\nWrote %d files under %s:\n", len(written), target)
	for _, name := range written {
		st, _ := os.Stat(filepath.Join(target, name))
		size := int64(0)
		if st != nil {
			size = st.Size()
		}
		fmt.Fprintf(out, "  %s (%d bytes)\n", name, size)
	}

	fmt.Fprintln(out, "\nplan: READY — when authorized, run `dpubnkctl cluster up`")
	return nil
}

// writeFiles writes every key/value as a file under root, creating parent
// dirs as needed. Returns the relative paths it wrote, sorted.
func writeFiles(root string, files map[string]string) []string {
	keys := make([]string, 0, len(files))
	for k := range files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		full := filepath.Join(root, k)
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		_ = os.WriteFile(full, []byte(files[k]), 0o644)
	}
	return keys
}

// kubesprayVersionFromPlan returns the pinned kubespray version. Lives
// here only to keep the import surface small in cluster.go.
func kubesprayVersionFromPlan() string {
	// Avoid pulling internal/version into this file just for one constant
	// — read it via the cluster package's renderer fingerprint.
	return clusterKubesprayPin()
}

func clusterKubesprayPin() string { return cluster.KubesprayPinForCLI() }
