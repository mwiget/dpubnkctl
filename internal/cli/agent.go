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
}

func newAgentCmd() *cobra.Command {
	var pocDir string
	cmd := &cobra.Command{
		Use:   "agent [claude|gemini|aider|openai]",
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

			// Printing an invocation needs the PoC repo path.
			if repoErr != nil {
				return repoErr
			}
			if p == nil {
				return fmt.Errorf("not a PoC repo (%s) — cd into a `dpubnkctl init`-created repo or pass --poc", repo)
			}
			recipe, ok := agentRecipes[args[0]]
			if !ok {
				return fmt.Errorf("unknown agent %q (try: claude, gemini, aider, openai)", args[0])
			}
			fmt.Fprint(out, recipe(repo, endpoint))
			return nil
		},
	}
	cmd.Flags().StringVar(&pocDir, "poc", "", "PoC repo path (default: current directory)")
	cmd.Flags().String("llm-endpoint", "", "OpenAI-compatible / Anthropic-compatible base URL (overrides poc.yaml)")
	return cmd
}

// resolvePoCDir returns the PoC directory to use, defaulting to cwd.
// It does not validate that poc.yaml exists; the caller does that via Load.
func resolvePoCDir(flag string) (string, error) {
	if flag != "" {
		return filepath.Abs(flag)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return cwd, nil
}
