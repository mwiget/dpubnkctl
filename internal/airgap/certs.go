package airgap

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

func GenerateCerts(ctx context.Context, w io.Writer, certDir, jumphostIP string) error {
	keyPath := filepath.Join(certDir, "registry.key")
	crtPath := filepath.Join(certDir, "registry.crt")

	if _, err := os.Stat(crtPath); err == nil {
		fmt.Fprintln(w, "TLS certs already exist, skipping generation")
		return nil
	}

	if err := os.MkdirAll(certDir, 0o755); err != nil {
		return fmt.Errorf("create cert dir: %w", err)
	}

	fmt.Fprintf(w, "generating self-signed TLS certs for %s ...\n", jumphostIP)
	cmd := exec.CommandContext(ctx, "openssl", "req",
		"-x509", "-newkey", "rsa:4096",
		"-keyout", keyPath,
		"-out", crtPath,
		"-days", "365",
		"-nodes",
		"-subj", fmt.Sprintf("/CN=%s", jumphostIP),
		"-addext", fmt.Sprintf("subjectAltName=IP:%s", jumphostIP),
	)
	cmd.Stdout = w
	cmd.Stderr = w
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("openssl cert generation: %w", err)
	}
	return nil
}
