// Package ctlschema introspects a cobra command tree into a stable JSON
// document describing the CLI surface: every command's path, help text, and
// flags. It is deliberately CLI-surface-only — it reads nothing but the
// cobra/pflag metadata already declared for the human CLI, and knows nothing
// about MCP, agents, or the consumer that turns this into a tool catalog.
//
// The output is consumed out-of-band (e.g. by bnk-forge's catalog generator)
// and merged with a hand-authored annotation layer that adds semantics
// (mutating / irreversible / plan-only). Keeping that split here preserves the
// invariant that dpubnkctl never learns about the agent layer.
package ctlschema

import (
	"sort"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// SchemaVersion is the contract version of the emitted document. Bump it when
// the shape changes in a way a consumer must notice.
const SchemaVersion = 1

// CtlSchema is the top-level introspection document.
type CtlSchema struct {
	Ctl           string    `json:"ctl"`
	CtlVersion    string    `json:"ctlVersion"`
	BNKVersion    string    `json:"bnkVersion"`
	SchemaVersion int       `json:"schemaVersion"`
	Commands      []Command `json:"commands"`
}

// Command is one node of the cobra tree (including non-runnable parents).
type Command struct {
	Path     string   `json:"path"`     // full path, e.g. "destroy dpus" (excludes the root program name)
	Short    string   `json:"short"`    //
	Long     string   `json:"long"`     //
	Hidden   bool     `json:"hidden"`   // hidden from --help
	Runnable bool     `json:"runnable"` // has a Run/RunE (a leaf op vs a grouping parent)
	Aliases  []string `json:"aliases"`  //
	Flags    []Flag   `json:"flags"`    // local + inherited-persistent, deduped, sorted
}

// Flag describes one pflag on a command.
type Flag struct {
	Name      string `json:"name"`
	Shorthand string `json:"shorthand,omitempty"`
	Type      string `json:"type"`    // pflag Value.Type(): bool, string, duration, int, stringSlice, ...
	Default   string `json:"default"` // pflag DefValue
	Usage     string `json:"usage"`
	Required  bool   `json:"required"`
}

// Meta carries the identifying/version fields the walker cannot derive from
// the cobra tree itself.
type Meta struct {
	Ctl        string
	CtlVersion string
	BNKVersion string
}

// Walk introspects the command tree rooted at root and returns a stable
// document. The root command itself is not emitted as a Command; its children
// (and their descendants) are, keyed by path relative to the root name.
//
// The cobra-injected "help" and "completion" commands are skipped — they are
// framework scaffolding, not part of the tool's surface.
func Walk(root *cobra.Command, meta Meta) CtlSchema {
	var cmds []Command
	var recurse func(c *cobra.Command)
	recurse = func(c *cobra.Command) {
		for _, child := range c.Commands() {
			if isFrameworkCommand(child) {
				continue
			}
			cmds = append(cmds, describe(child))
			recurse(child)
		}
	}
	recurse(root)

	sort.Slice(cmds, func(i, j int) bool { return cmds[i].Path < cmds[j].Path })

	return CtlSchema{
		Ctl:           meta.Ctl,
		CtlVersion:    meta.CtlVersion,
		BNKVersion:    meta.BNKVersion,
		SchemaVersion: SchemaVersion,
		Commands:      cmds,
	}
}

// isFrameworkCommand reports whether c is a cobra-generated scaffolding command
// (the auto "help" topic or the "completion" generator) rather than a real
// dpubnkctl subcommand.
func isFrameworkCommand(c *cobra.Command) bool {
	if c.IsAdditionalHelpTopicCommand() {
		return true
	}
	switch c.Name() {
	case "help", "completion":
		return true
	}
	return false
}

// describe extracts a single command's metadata. Path is the cobra CommandPath
// with the root program name stripped, so "dpubnkctl destroy dpus" -> "destroy
// dpus" — the form the annotation layer and tool names key on.
func describe(c *cobra.Command) Command {
	path := c.CommandPath()
	if root := c.Root(); root != nil {
		if prefix := root.Name() + " "; len(path) > len(prefix) && path[:len(prefix)] == prefix {
			path = path[len(prefix):]
		}
	}

	aliases := c.Aliases
	if aliases == nil {
		aliases = []string{}
	}

	return Command{
		Path:     path,
		Short:    c.Short,
		Long:     c.Long,
		Hidden:   c.Hidden,
		Runnable: c.Runnable(),
		Aliases:  aliases,
		Flags:    collectFlags(c),
	}
}

// collectFlags returns the union of a command's own flags (local + its own
// persistent) and the persistent flags inherited from ancestors, deduped by
// name and sorted. An operator can pass any of these on the command line, so
// the catalog needs the full set.
func collectFlags(c *cobra.Command) []Flag {
	seen := map[string]Flag{}
	add := func(f *pflag.Flag) {
		if f.Hidden {
			return
		}
		if _, ok := seen[f.Name]; ok {
			return
		}
		seen[f.Name] = Flag{
			Name:      f.Name,
			Shorthand: f.Shorthand,
			Type:      f.Value.Type(),
			Default:   f.DefValue,
			Usage:     f.Usage,
			Required:  isRequired(f),
		}
	}
	c.Flags().VisitAll(add)
	c.InheritedFlags().VisitAll(add)

	out := make([]Flag, 0, len(seen))
	for _, f := range seen {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// isRequired reads cobra's required-flag annotation (set by MarkFlagRequired).
func isRequired(f *pflag.Flag) bool {
	vals, ok := f.Annotations[cobra.BashCompOneRequiredFlag]
	return ok && len(vals) > 0 && vals[0] == "true"
}
