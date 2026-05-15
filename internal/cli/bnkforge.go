package cli

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/mwiget/dpubnkctl/internal/bnkforge"
	"github.com/mwiget/dpubnkctl/internal/poc"
)

func newBNKForgeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bnk-forge",
		Short: "Bring up a local bnk-forge stack + auto-create a project mirroring this PoC",
		Long: `Integrates a local bnk-forge installation
(https://github.com/sp-prod-field/bnk-forge — currently private) with a
dpubnkctl PoC. The bnk-forge repo must be cloned locally; reference its
path in poc.yaml under bnk_forge.repo_path.

What 'launch' does:
  1. Confirm bnk_forge.enabled = true in poc.yaml.
  2. Health-check https://localhost (or bnk_forge.url). If responsive,
     skip make. Otherwise run 'make deploy' in the bnk-forge clone and
     poll the health endpoint until ready.
  3. POST /api/auth/login (admin/changeme by default; override via
     bnk_forge.admin_password).
  4. If a project with the PoC's name already exists, no-op. Otherwise
     POST /api/projects with name + description derived from poc.yaml's
     metadata + versions blocks.
  5. Print the URL to open in a browser.

Future: 'launch' will optionally upload the PoC's kubeconfig as a
project credential. Today it just creates the project shell.`,
	}
	cmd.AddCommand(newBNKForgeLaunchCmd())
	return cmd
}

type bnkForgeLaunchFlags struct {
	pocDir string
}

func newBNKForgeLaunchCmd() *cobra.Command {
	f := &bnkForgeLaunchFlags{}
	cmd := &cobra.Command{
		Use:   "launch",
		Short: "Ensure bnk-forge is running + create the dpubnkctl-mirroring project",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBNKForgeLaunch(cmd.Context(), cmd.OutOrStdout(), f)
		},
	}
	cmd.Flags().StringVar(&f.pocDir, "poc", "", "PoC repo path (default: current directory)")
	return cmd
}

func runBNKForgeLaunch(ctx context.Context, out io.Writer, f *bnkForgeLaunchFlags) error {
	repo, err := resolvePoCDir(f.pocDir)
	if err != nil {
		return err
	}
	p, err := poc.Load(repo)
	if err != nil {
		return fmt.Errorf("not a PoC repo (%s): %w", repo, err)
	}
	if !p.BNKForge.Enabled {
		return errors.New("bnk_forge.enabled is false in poc.yaml — set it true to launch bnk-forge")
	}
	return LaunchBNKForge(ctx, out, repo, p)
}

// LaunchBNKForge runs the full bnk-forge integration for a loaded PoC:
// ensure the local stack is running, authenticate, ensure the project
// exists, ensure the cluster (kubeconfig) is registered. Exported so
// other phases (e.g. `cluster up`) can chain it after they finish.
//
// The caller is responsible for honouring p.BNKForge.Enabled; this
// function assumes it should run unconditionally (it's the caller's
// gate, not ours).
func LaunchBNKForge(ctx context.Context, out io.Writer, repo string, p *poc.PoC) error {
	cfg := bnkforge.Config{
		RepoPath:      p.BNKForge.RepoPath,
		URL:           p.BNKForge.URL,
		AdminUsername: p.BNKForge.AdminUsername,
		AdminPassword: p.BNKForge.AdminPassword,
	}.WithDefaults()

	fmt.Fprintf(out, "PoC:           %s\n", p.Metadata.Name)
	fmt.Fprintf(out, "bnk-forge URL: %s\n", cfg.URL)
	fmt.Fprintf(out, "Repo path:     %s\n\n", cfg.RepoPath)

	fmt.Fprintln(out, "[1/4] Checking that bnk-forge is running ...")
	if err := bnkforge.RequireRunning(ctx, cfg, out); err != nil {
		return err
	}

	fmt.Fprintln(out, "\n[2/4] Authenticating to bnk-forge API ...")
	cli := bnkforge.NewClient(cfg)
	if err := cli.Login(ctx, cfg.AdminUsername, cfg.AdminPassword); err != nil {
		return fmt.Errorf("login: %w", err)
	}
	fmt.Fprintln(out, "  ok (token cached for the rest of this run)")

	fmt.Fprintln(out, "\n[3/4] Ensuring a project named", p.Metadata.Name, "exists ...")
	projectID, found, err := cli.FindProjectByName(ctx, p.Metadata.Name)
	if err != nil {
		return fmt.Errorf("list projects: %w", err)
	}
	if found {
		fmt.Fprintf(out, "  Project %q already exists (id=%d) — leaving in place.\n",
			p.Metadata.Name, projectID)
	} else {
		project := buildProjectFromPoC(p)
		projectID, err = cli.CreateProject(ctx, project)
		if err != nil {
			return fmt.Errorf("create project: %w", err)
		}
		fmt.Fprintf(out, "  Created project %q (id=%d)\n", p.Metadata.Name, projectID)
	}

	fmt.Fprintln(out, "\n[4/4] Registering the cluster (kubeconfig) with the project ...")
	if err := ensureProjectCluster(ctx, out, cli, repo, p, projectID); err != nil {
		return fmt.Errorf("register cluster: %w", err)
	}

	fmt.Fprintf(out, "\nOpen: %s\n", cfg.URL)
	return nil
}

// ensureProjectCluster reads <repo>/artifacts/kubeconfig, base64-encodes
// it, and POSTs to bnk-forge so the project sees the cluster. Idempotent:
// if a cluster with the PoC's name already exists in the project, skip.
func ensureProjectCluster(ctx context.Context, out io.Writer, cli *bnkforge.Client, repo string, p *poc.PoC, projectID int) error {
	clusters, err := cli.ListProjectClusters(ctx, projectID)
	if err != nil {
		return err
	}
	for _, c := range clusters {
		if c.Name == p.Metadata.Name {
			fmt.Fprintf(out, "  Cluster %q already registered in project (id=%d) — leaving in place.\n",
				p.Metadata.Name, c.ID)
			return nil
		}
	}

	kubeconfigPath := filepath.Join(repo, "artifacts", "kubeconfig")
	body, err := os.ReadFile(kubeconfigPath)
	if err != nil {
		return fmt.Errorf("read kubeconfig %s: %w (run `dpubnkctl cluster up` first)", kubeconfigPath, err)
	}
	encoded := base64.StdEncoding.EncodeToString(body)

	region := p.Metadata.Customer
	id, err := cli.CreateProjectCluster(ctx, projectID, bnkforge.Cluster{
		Name:             p.Metadata.Name,
		Kubeconfig:       encoded,
		CloudProvider:    "on-prem",
		Region:           region,
		DefaultNamespace: "default",
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "  Registered cluster %q (id=%d). bnk-forge should now see the live nodes + BNK CRDs.\n",
		p.Metadata.Name, id)
	return nil
}

func buildProjectFromPoC(p *poc.PoC) bnkforge.Project {
	color := p.BNKForge.ProjectColor
	if color == "" {
		color = "#0a3a5c"
	}
	desc := fmt.Sprintf("Imported from dpubnkctl PoC %q (BNK %s, DOCA %s).",
		p.Metadata.Name, p.Metadata.BNKVersion, p.Versions.DOCA)
	region := p.Metadata.Customer
	return bnkforge.Project{
		Name:                  p.Metadata.Name,
		Description:           desc,
		ProjectType:           "kubernetes",
		CloudProvider:         "on-prem",
		Environment:           "dev",
		Region:                region,
		TargetPlatformProfile: "generic_onprem",
		Color:                 color,
		Icon:                  p.BNKForge.ProjectIcon,
	}
}
