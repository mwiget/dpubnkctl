package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mwiget/dpubnkctl/examples"
)

// newSamplesCmd assembles `dpubnkctl samples` and its subcommands:
//
//	dpubnkctl samples                     → list (default behaviour)
//	dpubnkctl samples list                → list
//	dpubnkctl samples show <name>         → print the sample to stdout
//	dpubnkctl samples extract <name>      → write the sample to ./poc.yaml
//
// Samples are curated, validate-clean poc.yaml templates embedded in
// the binary. Source lives at examples/ in the repo so operators can
// also browse them on GitHub.
func newSamplesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "samples",
		Short: "List/show/extract embedded poc.yaml templates (jumpstart for new PoCs)",
		Long: `dpubnkctl ships a handful of validate-clean poc.yaml templates inside
the binary. Use them as a starting point for a new PoC instead of
filling a stub from scratch.

  dpubnkctl samples                       list every sample
  dpubnkctl samples show <name>           print the YAML to stdout
  dpubnkctl samples extract <name>        write to ./poc.yaml

After extracting, edit every line marked ` + "`# CUSTOMIZE:`" + ` to match
your environment, then run ` + "`dpubnkctl validate`" + `.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSamplesList(cmd.OutOrStdout())
		},
	}
	cmd.AddCommand(newSamplesListCmd())
	cmd.AddCommand(newSamplesShowCmd())
	cmd.AddCommand(newSamplesExtractCmd())
	return cmd
}

func newSamplesListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List every embedded sample, one per line, with its description",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSamplesList(cmd.OutOrStdout())
		},
	}
}

func runSamplesList(out io.Writer) error {
	all := examples.All()
	if len(all) == 0 {
		return fmt.Errorf("no samples embedded in this build")
	}
	// Compute the widest name so the description column lines up.
	width := 0
	for _, s := range all {
		if len(s.Name) > width {
			width = len(s.Name)
		}
	}
	for _, s := range all {
		fmt.Fprintf(out, "  %-*s   %s\n", width, s.Name, s.Description)
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Extract one as a starting point:")
	fmt.Fprintln(out, "  dpubnkctl samples extract <name>           # writes ./poc.yaml")
	fmt.Fprintln(out, "  dpubnkctl samples extract <name> --to X    # writes X")
	fmt.Fprintln(out, "  dpubnkctl samples show <name>              # print to stdout")
	return nil
}

func newSamplesShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <name>",
		Short: "Print an embedded sample's poc.yaml to stdout",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s := examples.Find(args[0])
			if s == nil {
				return examples.ErrNotFound(args[0])
			}
			_, err := io.WriteString(cmd.OutOrStdout(), s.Body)
			return err
		},
	}
}

type samplesExtractFlags struct {
	to    string
	force bool
}

func newSamplesExtractCmd() *cobra.Command {
	f := &samplesExtractFlags{}
	cmd := &cobra.Command{
		Use:   "extract <name>",
		Short: "Write an embedded sample to disk as a starting point",
		Long: `Copy the named sample to disk. Default destination is ./poc.yaml so
you can drop into a freshly-created PoC repo and start editing.

Refuses to overwrite an existing destination — pass --force to
clobber. Without --force, an existing destination is a hard error so
nothing in-progress gets accidentally clobbered.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSamplesExtract(cmd.OutOrStdout(), args[0], f)
		},
	}
	cmd.Flags().StringVar(&f.to, "to", "poc.yaml", "Destination path")
	cmd.Flags().BoolVar(&f.force, "force", false, "Overwrite an existing destination")
	return cmd
}

func runSamplesExtract(out io.Writer, name string, f *samplesExtractFlags) error {
	s := examples.Find(name)
	if s == nil {
		return examples.ErrNotFound(name)
	}
	dst := f.to
	if !filepath.IsAbs(dst) {
		// Resolve so the next-steps message shows where the file
		// actually landed rather than a possibly-confusing relative path.
		abs, err := filepath.Abs(dst)
		if err == nil {
			dst = abs
		}
	}
	if _, err := os.Stat(dst); err == nil && !f.force {
		return fmt.Errorf("%s already exists — pass --force to overwrite", dst)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(dst, []byte(s.Body), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(out, "Wrote %s from sample %q.\n\n", dst, name)
	fmt.Fprintln(out, "Next:")
	fmt.Fprintln(out, "  1. Edit every line marked `# CUSTOMIZE:` (operator-required values).")
	fmt.Fprintln(out, "  2. dpubnkctl validate              # confirm the schema is happy")
	fmt.Fprintln(out, "  3. Drop FAR + JWT into keys/, the SSH key into keys/, then:")
	fmt.Fprintln(out, "     dpubnkctl e2e --yolo            # full pipeline")
	return nil
}

// samplesNamesHint formats a comma-separated list of available
// samples for use in operator-facing errors. Re-exported as a
// helper so init's --sample integration shows the same string.
func samplesNamesHint() string {
	names := examples.Names()
	if len(names) == 0 {
		return "(no samples embedded)"
	}
	return strings.Join(names, ", ")
}
