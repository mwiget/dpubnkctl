package provision

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/mwiget/dpubnkctl/internal/version"
)

// sha256Hex returns the hex digest of b — the value verifyBFBChecksum
// compares against.
func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// writeTemp writes content to a temp file and returns its path.
func writeTemp(t *testing.T, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake.bfb")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestVerifyBFBChecksum covers the three cases the download + host paths
// rely on: an empty want is a no-op (unpinned), a matching digest passes,
// and a mismatch fails loud.
func TestVerifyBFBChecksum(t *testing.T) {
	content := []byte("pretend this is a BFB")
	path := writeTemp(t, content)
	good := sha256Hex(content)

	t.Run("unpinned (empty want) is a no-op", func(t *testing.T) {
		if err := verifyBFBChecksum(path, ""); err != nil {
			t.Errorf("empty want should noop, got: %v", err)
		}
	})
	t.Run("matching digest passes", func(t *testing.T) {
		if err := verifyBFBChecksum(path, good); err != nil {
			t.Errorf("matching digest should pass, got: %v", err)
		}
	})
	t.Run("uppercase digest still matches (case-insensitive)", func(t *testing.T) {
		if err := verifyBFBChecksum(path, upper(good)); err != nil {
			t.Errorf("uppercase digest should match, got: %v", err)
		}
	})
	t.Run("mismatch fails", func(t *testing.T) {
		wrong := sha256Hex([]byte("a different file"))
		if err := verifyBFBChecksum(path, wrong); err == nil {
			t.Error("mismatched digest should fail, got nil")
		}
	})
}

func upper(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'a' && b[i] <= 'f' {
			b[i] -= 'a' - 'A'
		}
	}
	return string(b)
}

// TestExpectedBFBSHA256 pins the precedence: poc override wins, else the
// binary pin, else empty (unpinned).
func TestExpectedBFBSHA256(t *testing.T) {
	pocSHA := sha256Hex([]byte("poc-pinned"))
	t.Run("poc override wins over binary pin", func(t *testing.T) {
		if got := ExpectedBFBSHA256(pocSHA); got != pocSHA {
			t.Errorf("ExpectedBFBSHA256(poc) = %q, want %q", got, pocSHA)
		}
	})
	t.Run("empty poc falls back to binary pin", func(t *testing.T) {
		if got := ExpectedBFBSHA256(""); got != version.BFBImageSHA256 {
			t.Errorf("ExpectedBFBSHA256(\"\") = %q, want binary pin %q", got, version.BFBImageSHA256)
		}
	})
	t.Run("whitespace-only poc falls back", func(t *testing.T) {
		if got := ExpectedBFBSHA256("   "); got != version.BFBImageSHA256 {
			t.Errorf("ExpectedBFBSHA256(spaces) = %q, want binary pin", got)
		}
	})
}

// TestParseSHA256SumOutput exercises the coreutils shapes plus the error
// cases (a truncated/error line must not read as a valid digest).
func TestParseSHA256SumOutput(t *testing.T) {
	digest := sha256Hex([]byte("x"))
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"two-space coreutils form", digest + "  /var/cache/x.bfb", digest, false},
		{"single-space form", digest + " /path", digest, false},
		{"binary-mode star marker", digest + " *" + "/path", digest, false},
		{"trailing newline + spaces", "  " + digest + "  /path\n", digest, false},
		{"uppercase normalised to lower", upper(digest) + "  /p", digest, false},
		{"error line fails", "sha256sum: /path: No such file or directory", "", true},
		{"empty fails", "", "", true},
		{"short hex fails", "deadbeef  /p", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseSHA256SumOutput(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error for %q, got %q", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error for %q: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("ParseSHA256SumOutput(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestBFBImageSHA256IsValidHex guards the shipped pin — a typo'd digest
// would fail every download + host verify, so catch it at test time.
func TestBFBImageSHA256IsValidHex(t *testing.T) {
	if version.BFBImageSHA256 == "" {
		t.Skip("pin intentionally empty")
	}
	if !isHex64(version.BFBImageSHA256) {
		t.Fatalf("version.BFBImageSHA256 = %q is not a 64-char hex digest", version.BFBImageSHA256)
	}
}

// TestBFBDownloadURL: override wins verbatim, else base+image.
func TestBFBDownloadURL(t *testing.T) {
	if got := BFBDownloadURL("https://mirror/x.bfb", "img.bfb"); got != "https://mirror/x.bfb" {
		t.Errorf("override should be used verbatim, got %q", got)
	}
	got := BFBDownloadURL("", "bf-bundle.bfb")
	want := version.BFBBaseURL + "/bf-bundle.bfb"
	if got != want {
		t.Errorf("BFBDownloadURL(\"\", img) = %q, want %q", got, want)
	}
}
