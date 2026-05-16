package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/mwiget/dpubnkctl/internal/cluster"
	"github.com/mwiget/dpubnkctl/internal/poc"
)

type clusterResetFlags struct {
	pocDir         string
	yolo           bool
	confirmCluster string
	timeout        time.Duration
}

func newClusterResetCmd() *cobra.Command {
	f := &clusterResetFlags{}
	cmd := &cobra.Command{
		Use:   "reset",
		Short: "Run kubespray reset.yml to undo any kubespray-installed state (DESTRUCTIVE)",
		Long: `Run kubespray reset.yml against every host in poc.yaml. This:

  - stops kubelet/etcd/containerd/kube-apiserver
  - removes /etc/kubernetes, /var/lib/etcd, /var/lib/kubelet,
    cluster CRI sockets, kubespray-managed CNI config
  - leaves the host's OS otherwise intact (and the DPU OS untouched)

Use this before retrying 'cluster up' on hosts that have leftover state
from a prior k8s install. Required gates:

  --yolo                   acknowledge that this is destructive
  --confirm-cluster NAME   must equal poc.yaml.metadata.name (typo guard)`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runClusterReset(cmd.Context(), cmd.OutOrStdout(), f)
		},
	}
	cmd.Flags().StringVar(&f.pocDir, "poc", "", "PoC repo path (default: current directory)")
	cmd.Flags().BoolVar(&f.yolo, "yolo", false, "Acknowledge that this command is destructive")
	cmd.Flags().StringVar(&f.confirmCluster, "confirm-cluster", "", "Must equal poc.yaml.metadata.name (typo guard)")
	cmd.Flags().DurationVar(&f.timeout, "timeout", 30*time.Minute, "Wall-clock timeout for the reset run")
	return cmd
}

func runClusterReset(ctx context.Context, out io.Writer, f *clusterResetFlags) error {
	repo, err := resolvePoCDir(f.pocDir)
	if err != nil {
		return err
	}
	p, err := poc.Load(repo)
	if err != nil {
		return fmt.Errorf("not a PoC repo (%s): %w", repo, err)
	}

	if !f.yolo {
		return errors.New("refusing destructive reset without --yolo")
	}
	if f.confirmCluster != p.Metadata.Name {
		return fmt.Errorf("--confirm-cluster must equal poc.yaml.metadata.name (%q), got %q", p.Metadata.Name, f.confirmCluster)
	}
	if err := enforceValidateForPhase(out, p, repo, poc.PhaseCluster, false); err != nil {
		return err
	}

	plan := cluster.BuildPlan(p)
	if !plan.Valid() {
		for _, e := range plan.Errors {
			fmt.Fprintln(out, "  ✗", e)
		}
		return fmt.Errorf("plan is not valid")
	}

	fmt.Fprintf(out, "PoC: %s — running kubespray reset.yml against %v\n\n", p.Metadata.Name, allHostsInPlan(plan))

	if _, err := cluster.CheckContainerRuntime(ctx); err != nil {
		return err
	}

	files, err := cluster.Render(p, plan)
	if err != nil {
		return err
	}
	invDir := filepath.Join(repo, "artifacts", "kubespray-inventory")
	for name, content := range files {
		full := filepath.Join(invDir, name)
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			return err
		}
	}
	keysDir := filepath.Join(invDir, "keys")
	if err := os.MkdirAll(keysDir, 0o700); err != nil {
		return err
	}
	for hostName, h := range plan.HostByName {
		src := h.SSH.KeyRef
		if !filepath.IsAbs(src) {
			src = filepath.Join(repo, src)
		}
		data, err := os.ReadFile(src)
		if err != nil {
			return fmt.Errorf("read ssh key for %s (%s): %w", hostName, src, err)
		}
		if err := os.WriteFile(filepath.Join(keysDir, hostName+".pem"), data, 0o600); err != nil {
			return err
		}
	}

	logPath := filepath.Join(repo, "artifacts", "cluster-reset.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return err
	}
	defer logFile.Close()

	tee := io.MultiWriter(prefixWriter{w: out, prefix: "      | "}, logFile)
	exit, runErr := cluster.RunKubespray(ctx, cluster.RunOptions{
		InventoryDir: invDir,
		Out:          tee,
		Timeout:      f.timeout,
		Playbook:     "reset.yml",
		// Skip kubespray's interactive "Are you sure?" prompt.
		ExtraArgs: []string{"-e", "reset_confirmation=yes"},
	})
	if runErr != nil {
		return fmt.Errorf("kubespray reset: %w", runErr)
	}
	if exit != 0 {
		return fmt.Errorf("kubespray reset exited %d — see %s", exit, logPath)
	}
	fmt.Fprintf(out, "      reset completed — log at %s\n", logPath)

	p.Status.Cluster = "pending"
	if err := savePoC(repo, p, out); err != nil {
		return err
	}
	fmt.Fprintln(out, "\nDONE.  Re-run `dpubnkctl cluster up ...` to bring up a fresh cluster.")
	return nil
}

func allHostsInPlan(plan cluster.Plan) []string {
	seen := map[string]bool{}
	var out []string
	add := func(names []string) {
		for _, n := range names {
			if !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	add(plan.ControlPlane)
	add(plan.Workers)
	return out
}
