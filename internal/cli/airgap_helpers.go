package cli

import (
	"context"
	"io"
	"os/exec"
	"strings"
)

func execCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, name, args...)
}

func stringReader(s string) io.Reader {
	return strings.NewReader(s)
}
