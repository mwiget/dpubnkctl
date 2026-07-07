package provision

import (
	"context"
	"strings"
	"testing"

	"github.com/mwiget/dpubnkctl/internal/poc"
	"github.com/mwiget/dpubnkctl/internal/ssh"
)

// fakeRunner records the last command and replays a canned Result.
type fakeRunner struct {
	lastCmd string
	res     ssh.Result
}

func (f *fakeRunner) Run(_ context.Context, cmd string) ssh.Result {
	f.lastCmd = cmd
	return f.res
}

func TestBFBHostPath(t *testing.T) {
	if got := BFBHostPath("", "img.bfb"); got != poc.DefaultBFBHostCacheDir+"/img.bfb" {
		t.Errorf("default cacheDir: got %q", got)
	}
	if got := BFBHostPath("/srv/bfb", "img.bfb"); got != "/srv/bfb/img.bfb" {
		t.Errorf("explicit cacheDir: got %q", got)
	}
	if got := BFBHostPath("/srv/bfb/", "img.bfb"); got != "/srv/bfb/img.bfb" {
		t.Errorf("trailing slash: got %q", got)
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"/var/cache/x.bfb": `'/var/cache/x.bfb'`,
		"a b":              `'a b'`,
		"it's":             `'it'\''s'`, // embedded single quote neutralised
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHostCommandsAreQuoted(t *testing.T) {
	sha := HostSHA256Command("/var/cache/dpubnkctl/bfb/x.bfb")
	if !strings.Contains(sha, "sudo -n sha256sum '/var/cache/dpubnkctl/bfb/x.bfb'") {
		t.Errorf("HostSHA256Command not quoted as expected: %q", sha)
	}

	fetch := HostFetchCommand("https://host/x.bfb", "/var/cache/dpubnkctl/bfb/x.bfb")
	for _, want := range []string{
		"mkdir -p '/var/cache/dpubnkctl/bfb'",
		"-o '/var/cache/dpubnkctl/bfb/x.bfb.part'",
		"'https://host/x.bfb'",
		"mv -f '/var/cache/dpubnkctl/bfb/x.bfb.part' '/var/cache/dpubnkctl/bfb/x.bfb'",
		"--retry 5",
	} {
		if !strings.Contains(fetch, want) {
			t.Errorf("HostFetchCommand missing %q in:\n%s", want, fetch)
		}
	}
}

func TestRemoteSHA256(t *testing.T) {
	digest := sha256Hex([]byte("x"))

	t.Run("parses digest on success", func(t *testing.T) {
		r := &fakeRunner{res: ssh.Result{Stdout: digest + "  /var/cache/x.bfb", ExitCode: 0}}
		got, err := RemoteSHA256(context.Background(), r, "/var/cache/x.bfb")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != digest {
			t.Errorf("got %q, want %q", got, digest)
		}
		if !strings.Contains(r.lastCmd, "sha256sum") {
			t.Errorf("ran unexpected command: %q", r.lastCmd)
		}
	})

	t.Run("nonzero exit (missing file) errors", func(t *testing.T) {
		r := &fakeRunner{res: ssh.Result{Stderr: "sha256sum: x: No such file or directory", ExitCode: 1}}
		if _, err := RemoteSHA256(context.Background(), r, "/nope"); err == nil {
			t.Error("expected error on nonzero exit, got nil")
		}
	})
}
