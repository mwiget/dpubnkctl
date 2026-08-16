package airgap

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

func StartRegistry(ctx context.Context, w io.Writer, certDir string) error {
	if containerRunning(ctx, RegistryContainer) {
		fmt.Fprintln(w, "registry container already running")
		return nil
	}

	_ = exec.CommandContext(ctx, "docker", "rm", "-f", RegistryContainer).Run()

	fmt.Fprintf(w, "starting local registry on port %d ...\n", RegistryPort)
	cmd := exec.CommandContext(ctx, "docker", "run", "-d",
		"--name", RegistryContainer,
		"--restart=always",
		"-p", fmt.Sprintf("%d:5000", RegistryPort),
		"-v", certDir+":/certs",
		"-e", "REGISTRY_HTTP_TLS_CERTIFICATE=/certs/registry.crt",
		"-e", "REGISTRY_HTTP_TLS_KEY=/certs/registry.key",
		RegistryImage,
	)
	cmd.Stdout = w
	cmd.Stderr = w
	return cmd.Run()
}

func StopRegistry(ctx context.Context, w io.Writer) error {
	fmt.Fprintln(w, "stopping registry container ...")
	cmd := exec.CommandContext(ctx, "docker", "rm", "-f", RegistryContainer)
	cmd.Stdout = w
	cmd.Stderr = w
	return cmd.Run()
}

func PushImage(ctx context.Context, w io.Writer, tarPath, registryRef string) error {
	cmd := exec.CommandContext(ctx, "skopeo", "copy",
		"--dest-tls-verify=false",
		"docker-archive:"+tarPath,
		"docker://"+registryRef,
	)
	cmd.Stdout = w
	cmd.Stderr = w
	return cmd.Run()
}

func PullAndPush(ctx context.Context, w io.Writer, srcRef, tarPath, registryRef, arch string) error {
	os.Remove(tarPath)
	pull := exec.CommandContext(ctx, "skopeo", "copy",
		"--override-arch", arch,
		"docker://"+srcRef,
		"docker-archive:"+tarPath+":"+srcRef,
	)
	pull.Stdout = w
	pull.Stderr = w
	if err := pull.Run(); err != nil {
		return fmt.Errorf("skopeo pull %s: %w", srcRef, err)
	}
	return PushImage(ctx, w, tarPath, registryRef)
}

func PullAndPushWithAuth(ctx context.Context, w io.Writer, srcRef, tarPath, registryRef, arch string) error {
	return PullAndPush(ctx, w, srcRef, tarPath, registryRef, arch)
}

func PullToTar(ctx context.Context, w io.Writer, srcRef, tarPath, arch string) error {
	os.Remove(tarPath)
	cmd := exec.CommandContext(ctx, "skopeo", "copy",
		"--override-arch", arch,
		"docker://"+srcRef,
		"docker-archive:"+tarPath+":"+srcRef,
	)
	cmd.Stdout = w
	cmd.Stderr = w
	return cmd.Run()
}

func StripRegistryPrefix(imageRef string) string {
	prefixes := []string{
		"registry.k8s.io/",
		"quay.io/",
		"docker.io/",
		"ghcr.io/",
		"repo.f5.com/",
	}
	for _, p := range prefixes {
		if strings.HasPrefix(imageRef, p) {
			return strings.TrimPrefix(imageRef, p)
		}
	}
	return imageRef
}

func containerRunning(ctx context.Context, name string) bool {
	out, err := exec.CommandContext(ctx, "docker", "inspect",
		"--format", "{{.State.Running}}", name).Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}
