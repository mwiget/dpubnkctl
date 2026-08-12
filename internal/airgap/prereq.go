package airgap

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"time"
)

func CheckPrerequisites(ctx context.Context, w io.Writer, mode, farKeyPath string) error {
	fmt.Fprintln(w, "checking prerequisites ...")
	var errs []string

	if err := checkDocker(ctx); err != nil {
		errs = append(errs, fmt.Sprintf("docker: %v", err))
	} else {
		fmt.Fprintln(w, "  docker: ok")
	}

	if err := checkSkopeo(ctx); err != nil {
		errs = append(errs, fmt.Sprintf("skopeo: %v", err))
	} else {
		fmt.Fprintln(w, "  skopeo: ok")
	}

	if err := checkPort(RegistryPort); err != nil {
		errs = append(errs, fmt.Sprintf("port %d: %v", RegistryPort, err))
	} else {
		fmt.Fprintf(w, "  port %d: available\n", RegistryPort)
	}

	if err := checkPort(FileServerPort); err != nil {
		errs = append(errs, fmt.Sprintf("port %d: %v", FileServerPort, err))
	} else {
		fmt.Fprintf(w, "  port %d: available\n", FileServerPort)
	}

	if mode == ModeOnline {
		if err := checkFARKey(farKeyPath); err != nil {
			errs = append(errs, fmt.Sprintf("FAR key: %v", err))
		} else {
			fmt.Fprintln(w, "  FAR key: found")
		}

		if err := checkInternet(ctx); err != nil {
			errs = append(errs, fmt.Sprintf("internet: %v", err))
		} else {
			fmt.Fprintln(w, "  internet: reachable")
		}
	}

	if mode == ModeOffline {
		fmt.Fprintln(w, "  mode: offline (internet check skipped)")
	}

	if len(errs) > 0 {
		fmt.Fprintln(w, "")
		for _, e := range errs {
			fmt.Fprintf(w, "  ✗ %s\n", e)
		}
		return fmt.Errorf("%d prerequisite(s) failed", len(errs))
	}

	fmt.Fprintln(w, "prerequisites OK")
	return nil
}

func checkDocker(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "docker", "info")
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("not running or not installed — run `curl -fsSL https://get.docker.com | sh`")
	}
	return nil
}

func checkSkopeo(ctx context.Context) error {
	_, err := exec.LookPath("skopeo")
	if err != nil {
		return fmt.Errorf("not installed — run `sudo apt-get install -y skopeo`")
	}
	return nil
}

func checkPort(port int) error {
	ln, err := net.Listen("tcp", ":"+strconv.Itoa(port))
	if err != nil {
		if containerRunning(context.Background(), containerNameForPort(port)) {
			return nil
		}
		return fmt.Errorf("already in use — run `docker rm -f $(docker ps -q --filter publish=%d)` or stop the service", port)
	}
	ln.Close()
	return nil
}

func containerNameForPort(port int) string {
	switch port {
	case RegistryPort:
		return RegistryContainer
	case FileServerPort:
		return FileServerContainer
	default:
		return ""
	}
}

func checkFARKey(path string) error {
	if path == "" {
		return fmt.Errorf("far_key_ref not set in poc.yaml")
	}
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("not found at %s", path)
	}
	return nil
}

func checkInternet(ctx context.Context) error {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("https://registry.k8s.io/v2/")
	if err != nil {
		return fmt.Errorf("cannot reach registry.k8s.io — check internet connectivity")
	}
	resp.Body.Close()
	return nil
}
