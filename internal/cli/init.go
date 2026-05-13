package cli

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mwiget/dpubnkctl/internal/embedded"
	"github.com/mwiget/dpubnkctl/internal/poc"
)

func newInitCmd() *cobra.Command {
	var (
		dir      string
		customer string
		noGit    bool
	)
	cmd := &cobra.Command{
		Use:   "init <poc-name>",
		Short: "Create a new PoC repo (declarative state + journal + agent personas)",
		Long: `Create a new PoC repo at ./<poc-name> (or --dir <path>).

The repo contains:
  poc.yaml       declarative state (source of truth for teardown/redeploy)
  AGENTS.md      instructions for any agentic CLI driving this PoC
  CLAUDE.md      one-line @AGENTS.md include for Claude Code
  personas/      pre-sales-se, lab-tech, doc-specialist role definitions
  journal/       append-only markdown log written during the PoC
  inventory/     populated by 'dpubnkctl discover'
  artifacts/     bf.conf renders, kubespray inventory, kubeconfig, ...
  keys/          gitignored — drop FAR tgz, JWT, SSH keys here
  decisions.md   running decision log (owned by pre-sales SE persona)
  .gitignore     excludes all secret material from git

Initializes a git repo unless --no-git.`,
		Args: initArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if !validName(name) {
				return fmt.Errorf("invalid PoC name %q: use [a-z0-9-]+", name)
			}
			target := dir
			if target == "" {
				target = "./" + name
			}
			// Honor `~` / `~/...` even when the shell didn't expand it
			// (quoted args, non-bash shells, some IDE terminals). Anything
			// past that is filepath.Abs's job.
			target = expandTilde(target)
			abs, err := filepath.Abs(target)
			if err != nil {
				return err
			}
			if _, err := os.Stat(abs); err == nil {
				return fmt.Errorf("refusing to overwrite: %s already exists", abs)
			}

			if err := os.MkdirAll(abs, 0o755); err != nil {
				return err
			}

			// Create skeleton dirs.
			for _, d := range []string{"journal", "inventory", "artifacts", "keys", "personas"} {
				if err := os.MkdirAll(filepath.Join(abs, d), 0o755); err != nil {
					return err
				}
			}

			// Drop a placeholder so empty dirs survive git add.
			for _, d := range []string{"journal", "inventory", "artifacts"} {
				keep := filepath.Join(abs, d, ".gitkeep")
				if err := os.WriteFile(keep, nil, 0o644); err != nil {
					return err
				}
			}

			// Copy embedded files (AGENTS.md, CLAUDE.md, personas/, .gitignore).
			if err := copyEmbedded(abs); err != nil {
				return fmt.Errorf("copy templates: %w", err)
			}

			// Write poc.yaml with binary defaults.
			p := poc.New(name)
			p.Metadata.Customer = customer
			if err := p.Save(abs); err != nil {
				return err
			}

			// Seed decisions.md with empty template.
			decisions := fmt.Sprintf(`# Decisions — %s

Running log of scope, topology, and tradeoff decisions made during this PoC.
Owned by the pre-sales SE persona.

| Date | Decision | Rationale | Alternative rejected |
|------|----------|-----------|----------------------|
| %s | PoC created with dpubnkctl defaults (BNK 2.2.0, DOCA 2.9.2, FLO v2.9.27-0.2.10) | binary-pinned baseline | manual stack composition |
`, name, time.Now().UTC().Format("2006-01-02"))
			if err := os.WriteFile(filepath.Join(abs, "decisions.md"), []byte(decisions), 0o644); err != nil {
				return err
			}

			// First journal entry.
			j := fmt.Sprintf(`# %s — PoC initialized

Created with dpubnkctl. Next step: pre-sales SE confirms scope and topology
in poc.yaml, then lab-tech runs `+"`dpubnkctl discover host <ip>`"+` for each
host the customer provides.
`, time.Now().UTC().Format("2006-01-02"))
			if err := os.WriteFile(filepath.Join(abs, "journal", time.Now().UTC().Format("2006-01-02")+"-init.md"), []byte(j), 0o644); err != nil {
				return err
			}

			if !noGit {
				if err := gitInit(abs); err != nil {
					return fmt.Errorf("git init: %w", err)
				}
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "PoC repo created at %s\n", abs)
			fmt.Fprintln(out, "Next:")
			fmt.Fprintf(out, "  cd %s\n", target)
			fmt.Fprintln(out, "  $EDITOR poc.yaml          # set topology, expected hosts, network plan")
			fmt.Fprintln(out, "  dpubnkctl discover host <ip> --ssh-user <u> --ssh-key keys/<file>")
			fmt.Fprintln(out, "Or drive it with an agent:")
			fmt.Fprintln(out, "  dpubnkctl agent claude    # prints invocation for Claude Code")
			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "", "target directory (default ./<poc-name>)")
	cmd.Flags().StringVar(&customer, "customer", "", "customer name to record in poc.yaml metadata")
	cmd.Flags().BoolVar(&noGit, "no-git", false, "skip git init")
	return cmd
}

// initArgs surfaces a friendlier error than cobra's default
// "accepts 1 arg(s), received 0" when the operator forgets to name the
// PoC. The first ask after install is `dpubnkctl init` and a cryptic
// arity error is a bad first impression.
func initArgs(cmd *cobra.Command, args []string) error {
	switch len(args) {
	case 0:
		return fmt.Errorf(`PoC name required.

Usage:
  dpubnkctl init <poc-name> [--dir <path>] [--customer "<name>"]

By default the PoC repo is created at ./<poc-name> relative to your
current working directory. Pass --dir to put it elsewhere.

Examples:
  dpubnkctl init customer-acme                           # → ./customer-acme
  dpubnkctl init customer-acme --customer "Acme Corp"    # → ./customer-acme
  dpubnkctl init lab1 --dir ~/pocs/lab1                  # → ~/pocs/lab1
  dpubnkctl init lab1 --dir /srv/pocs/lab1               # absolute path

The name must match [a-z0-9-]+ and lands in poc.yaml.metadata.name.
The target directory must not already exist (init refuses to overwrite).
Run "dpubnkctl init --help" for all flags`)
	case 1:
		return nil
	default:
		return fmt.Errorf("only one <poc-name> is accepted; got %d arguments", len(args))
	}
}

// expandTilde resolves a leading `~` or `~/...` to the operator's home
// directory. Anything else is returned unchanged. Mirrors what bash
// would do unquoted; covers the case where the path arrived quoted or
// from a non-bash shell.
func expandTilde(p string) string {
	if p == "~" {
		if h, err := os.UserHomeDir(); err == nil {
			return h
		}
		return p
	}
	if strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[2:])
		}
	}
	return p
}

func validName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-':
		default:
			return false
		}
	}
	return true
}

// copyEmbedded walks the embedded FS and writes everything under "files/"
// into target, with one rename: "files/poc.gitignore" → ".gitignore".
func copyEmbedded(target string) error {
	return fs.WalkDir(embedded.FS, "files", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(path, "files/")
		if rel == "" || rel == "files" {
			return nil
		}
		if rel == "poc.gitignore" {
			rel = ".gitignore"
		}
		dst := filepath.Join(target, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		data, err := embedded.FS.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		return os.WriteFile(dst, data, 0o644)
	})
}

func gitInit(dir string) error {
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"add", "."},
		{"commit", "-q", "-m", "dpubnkctl init"},
	} {
		c := exec.Command("git", args...)
		c.Dir = dir
		c.Stdout = os.Stderr
		c.Stderr = os.Stderr
		if err := c.Run(); err != nil {
			return fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
		}
	}
	return nil
}
