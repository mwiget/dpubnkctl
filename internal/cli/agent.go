package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/mwiget/dpubnkctl/internal/poc"
)

// Supported agentic CLIs. dpubnkctl does not embed an LLM; it prints the
// invocation the operator should run with their preferred CLI, all
// pointing at the PoC repo's AGENTS.md and personas/.
var agentRecipes = map[string]func(repo, endpoint string) string{
	"claude": func(repo, endpoint string) string {
		extra := ""
		if endpoint != "" {
			extra = "\n  ANTHROPIC_BASE_URL=" + endpoint + " \\"
		}
		return fmt.Sprintf(`# Claude Code (https://docs.claude.com/en/docs/claude-code)
cd %s && \%s
  claude
# Then say:
#   "Read AGENTS.md, then act as the pre-sales SE persona. Confirm scope with me."
`, repo, extra)
	},
	"gemini": func(repo, endpoint string) string {
		return fmt.Sprintf(`# Gemini CLI
cd %s && \
  gemini chat --system-instruction "$(cat AGENTS.md)"
# Then point Gemini at personas/pre-sales-se.md to start.
`, repo)
	},
	"aider": func(repo, endpoint string) string {
		base := ""
		if endpoint != "" {
			base = " --openai-api-base " + endpoint
		}
		return fmt.Sprintf(`# Aider
cd %s && \
  aider --read AGENTS.md --read personas/pre-sales-se.md%s \
        poc.yaml decisions.md
`, repo, base)
	},
	"openai": func(repo, endpoint string) string {
		base := endpoint
		if base == "" {
			base = "https://api.openai.com/v1  # or your local vLLM endpoint"
		}
		return fmt.Sprintf(`# Generic OpenAI-compatible REPL (e.g., llm, chatgpt-cli)
cd %s
export OPENAI_API_BASE=%s
# Load AGENTS.md as system prompt and start with personas/pre-sales-se.md.
# Example with simonw/llm:
#   llm --system "$(cat AGENTS.md)" "Act as pre-sales SE; read poc.yaml; confirm scope."
`, repo, base)
	},
	"pi": func(repo, endpoint string) string {
		// pi auto-discovers and concatenates AGENTS.md (and CLAUDE.md)
		// from cwd, parent dirs, and ~/.pi/agent/ — so just `cd` + `pi`
		// is enough. Endpoint isn't a built-in flag; pi reads provider
		// config from ~/.pi/. Install: curl -fsSL https://pi.dev/install.sh | sh
		return fmt.Sprintf(`# pi coding agent (https://pi.dev/)
cd %s && \
  pi
# pi auto-loads AGENTS.md from this directory (and parent dirs).
# To add the persona inline, prefix with --append-system-prompt:
#   pi --append-system-prompt "$(cat personas/pre-sales-se.md)"
`, repo)
	},
	"opencode": func(repo, endpoint string) string {
		// opencode (Go-based TUI, https://opencode.ai/) treats AGENTS.md
		// as its project configuration file by convention. Bare invocation
		// in the directory is enough — no flag for context loading.
		// NOTE: Anthropic blocked opencode from Claude models in Jan 2026;
		// use --model to pick a non-Anthropic provider, or rely on its
		// auto-detection of OPENAI_API_KEY / OPENROUTER_API_KEY / etc.
		return fmt.Sprintf(`# OpenCode (https://opencode.ai/)
cd %s && \
  opencode
# AGENTS.md in this dir is opencode's project-config convention — auto-loaded.
# Pick a model with --model (e.g. --model openrouter/google/gemini-2.5-pro).
# Anthropic Claude models are blocked for opencode since 2026-01.
`, repo)
	},
}

func newAgentCmd() *cobra.Command {
	var pocDir string
	cmd := &cobra.Command{
		Use:   "agent [claude|gemini|aider|openai|pi|opencode]",
		Short: "Print invocation for an agentic CLI driving this PoC repo",
		Long: `Print the command-line invocation for your preferred agentic CLI,
configured to load this PoC's AGENTS.md and persona files.

dpubnkctl does not embed an LLM. The operator chooses which CLI to use and
where its API endpoint is (cloud vendor, local vLLM, etc.) — set
--llm-endpoint or OPENAI_API_BASE / ANTHROPIC_BASE_URL in your environment.

Without an argument, lists all supported CLIs.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			endpoint, _ := cmd.Flags().GetString("llm-endpoint")

			// Resolve PoC + load poc.yaml, but only require it when the
			// caller is asking us to PRINT an invocation (needs repo path).
			// The bare listing form is global info — don't fail just
			// because cwd happens to not be a PoC repo.
			repo, repoErr := resolvePoCDir(pocDir)
			var p *poc.PoC
			if repoErr == nil {
				if loaded, err := poc.Load(repo); err == nil {
					p = loaded
					if endpoint == "" {
						endpoint = p.Agent.LLMEndpoint
					}
				}
			}

			if len(args) == 0 {
				fmt.Fprintln(out, "Supported agentic CLIs:")
				for name := range agentRecipes {
					fmt.Fprintf(out, "  - %s\n", name)
				}
				if p != nil {
					fmt.Fprintf(out, "\nDefault for this PoC: %s\n", p.Agent.Default)
				} else {
					fmt.Fprintln(out, "\n(not in a PoC repo — cd into one or pass --poc <dir> to see its default)")
				}
				fmt.Fprintln(out, "\nRun:  dpubnkctl agent <name>  to print its invocation.")
				return nil
			}

			// Printing an invocation just needs a directory to `cd` into.
			// Don't insist poc.yaml exists — the recipes work fine against
			// any directory that has an AGENTS.md (e.g. the dpubnkctl
			// source tree itself). When poc.yaml IS missing, drop a note
			// after the recipe so the operator knows.
			if repoErr != nil {
				return repoErr
			}
			recipe, ok := agentRecipes[args[0]]
			if !ok {
				return fmt.Errorf("unknown agent %q (try: claude, gemini, aider, openai, pi, opencode)", args[0])
			}
			fmt.Fprint(out, recipe(repo, endpoint))
			if p == nil {
				fmt.Fprintf(out, "\n# NOTE: %s has no poc.yaml — recipe was templated for this directory anyway.\n", repo)
				fmt.Fprintln(out, "# To target a PoC, pass --poc <dir> or cd into one created by `dpubnkctl init`.")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&pocDir, "poc", "", "PoC repo path (default: current directory)")
	cmd.Flags().String("llm-endpoint", "", "OpenAI-compatible / Anthropic-compatible base URL (overrides poc.yaml)")
	return cmd
}

// resolvePoCDir returns the PoC directory to use, defaulting to cwd.
// It does not validate that poc.yaml exists; the caller does that via Load.
// If the operator passed `--poc /path/to/poc.yaml` (the file rather than
// its containing dir), strip the trailing filename so Load doesn't end up
// looking for poc.yaml/poc.yaml.
func resolvePoCDir(flag string) (string, error) {
	if flag != "" {
		abs, err := filepath.Abs(flag)
		if err != nil {
			return "", err
		}
		if info, err := os.Stat(abs); err == nil && !info.IsDir() && filepath.Base(abs) == poc.FileName {
			abs = filepath.Dir(abs)
		}
		return abs, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return cwd, nil
}
