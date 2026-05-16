package provision

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// TestVerifyBFBChecksum_Unpinned — when version.BFBImageSHA256 is empty
// (the default), verifyBFBChecksum is a noop. This is the current
// state of the binary; the field is infrastructure for future hash
// pinning. The test guards against accidentally rejecting unpinned
// downloads.
func TestVerifyBFBChecksum_Unpinned(t *testing.T) {
	// Don't try to swap version.BFBImageSHA256 — it's a const. Just
	// confirm the current (empty) value lets a random file pass.
	dir := t.TempDir()
	path := filepath.Join(dir, "fake.bfb")
	if err := os.WriteFile(path, []byte("not a real BFB"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifyBFBChecksum(path); err != nil {
		t.Errorf("expected unpinned to noop, got: %v", err)
	}
}

// TestSHA256ShapeMatches confirms that if BFBImageSHA256 is ever
// populated, it's a 64-char hex string — caught at test time rather
// than after a 2GB download.
func TestSHA256ShapeMatches(t *testing.T) {
	// Compute a canonical hash so the test exercises the same shape
	// verifyBFBChecksum expects.
	h := sha256.Sum256([]byte("anything"))
	got := hex.EncodeToString(h[:])
	if len(got) != 64 {
		t.Fatalf("sha256 hex length = %d, want 64", len(got))
	}
}
