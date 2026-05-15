package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

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

	cfg := bnkforge.Config{
		RepoPath:      p.BNKForge.RepoPath,
		URL:           p.BNKForge.URL,
		AdminUsername: p.BNKForge.AdminUsername,
		AdminPassword: p.BNKForge.AdminPassword,
	}.WithDefaults()

	fmt.Fprintf(out, "PoC:           %s\n", p.Metadata.Name)
	fmt.Fprintf(out, "bnk-forge URL: %s\n", cfg.URL)
	fmt.Fprintf(out, "Repo path:     %s\n\n", cfg.RepoPath)

	fmt.Fprintln(out, "[1/3] Ensuring bnk-forge is running ...")
	if err := bnkforge.EnsureRunning(ctx, cfg, out); err != nil {
		return err
	}

	fmt.Fprintln(out, "\n[2/3] Authenticating to bnk-forge API ...")
	cli := bnkforge.NewClient(cfg)
	if err := cli.Login(ctx, cfg.AdminUsername, cfg.AdminPassword); err != nil {
		return fmt.Errorf("login: %w", err)
	}
	fmt.Fprintln(out, "  ok (token cached for the rest of this run)")

	fmt.Fprintln(out, "\n[3/3] Ensuring a project named", p.Metadata.Name, "exists ...")
	if id, found, err := cli.FindProjectByName(ctx, p.Metadata.Name); err != nil {
		return fmt.Errorf("list projects: %w", err)
	} else if found {
		fmt.Fprintf(out, "  Project %q already exists (id=%d) — leaving in place.\n",
			p.Metadata.Name, id)
		fmt.Fprintf(out, "\nOpen: %s\n", cfg.URL)
		return nil
	}

	project := buildProjectFromPoC(p)
	id, err := cli.CreateProject(ctx, project)
	if err != nil {
		return fmt.Errorf("create project: %w", err)
	}
	fmt.Fprintf(out, "  Created project %q (id=%d)\n", p.Metadata.Name, id)
	fmt.Fprintf(out, "\nOpen: %s\n", cfg.URL)
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
