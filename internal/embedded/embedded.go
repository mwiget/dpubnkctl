// Package embedded ships AGENTS.md, persona files, and the .gitignore
// template inside the binary so `dpubnkctl init` can drop them into a
// fresh PoC repo without depending on the source tree.
package embedded

import "embed"

//go:embed files
var FS embed.FS
