package cli

import (
	"fmt"
	"os"
	"path/filepath"
)

// requireKubeconfig stat-checks the local kubeconfig under
// artifacts/kubeconfig and returns its absolute path. If the file is
// missing, the error names the operator-facing recipe that creates it
// (runFirst is appended after a `— ` separator). Used by every deploy
// path and by `destroy bnk` — all need an apiserver connection and
// share the same failure shape.
//
// Centralised so the error message stays consistent ("kubeconfig X
// missing — run Y first") rather than the previous five sites each
// inventing their own phrasing.
func requireKubeconfig(repo, runFirst string) (string, error) {
	path := filepath.Join(repo, "artifacts", "kubeconfig")
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("kubeconfig %s missing — %s", path, runFirst)
	}
	return path, nil
}
