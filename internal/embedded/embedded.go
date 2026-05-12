// Package embedded ships static assets inside the binary.
//
// Two trees live here:
//
//   - files/      → copied into a fresh PoC repo by `dpubnkctl init`
//                  (AGENTS.md, personas, .gitignore template).
//
//   - templates/  → binary-internal — bf.conf templates and similar
//                  rendering inputs. NOT copied to PoC repos.
package embedded

import "embed"

//go:embed files
var FS embed.FS

//go:embed templates
var Templates embed.FS
