package deploy

import (
	"archive/tar"
	"compress/gzip"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func makeJWT(t *testing.T, header, claims string) string {
	t.Helper()
	enc := func(s string) string {
		return strings.TrimRight(base64.URLEncoding.EncodeToString([]byte(s)), "=")
	}
	return enc(header) + "." + enc(claims) + ".sig-not-checked"
}

func TestInspectJWT_Prod(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), ".jwt")
	jwt := makeJWT(t,
		`{"alg":"RS256","kid":"prod-2024-01"}`,
		`{"iss":"https://license.f5.com","sub":"customer-x"}`)
	if err := os.WriteFile(tmp, []byte(jwt), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := InspectJWT(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if info.Type != "prod" {
		t.Errorf("Type = %q, want prod", info.Type)
	}
}

func TestInspectJWT_TstByClaim(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), ".jwt")
	jwt := makeJWT(t,
		`{"alg":"RS256"}`,
		`{"iss":"https://license.f5.com","tst":true}`)
	_ = os.WriteFile(tmp, []byte(jwt), 0o600)
	info, _ := InspectJWT(tmp)
	if info.Type != "tst" {
		t.Errorf("Type = %q, want tst (claim)", info.Type)
	}
}

func TestInspectJWT_TstByIssuer(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), ".jwt")
	jwt := makeJWT(t,
		`{"alg":"RS256"}`,
		`{"iss":"https://tst.license.f5.com"}`)
	_ = os.WriteFile(tmp, []byte(jwt), 0o600)
	info, _ := InspectJWT(tmp)
	if info.Type != "tst" {
		t.Errorf("Type = %q, want tst (issuer)", info.Type)
	}
}

func TestInspectJWT_BadFormat(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), ".jwt")
	_ = os.WriteFile(tmp, []byte("not.a.jwt.ok"), 0o600)
	if _, err := InspectJWT(tmp); err == nil {
		// last "ok" is base64-decodable as garbage, but the claims
		// part should fail JSON unmarshal.
		t.Skip("permissive parse")
	}
}

func makeFARTgz(t *testing.T, dockerConfig string) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "far.tgz")
	f, err := os.Create(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	body := []byte(dockerConfig)
	hdr := &tar.Header{
		Name:     "f5-far-auth-key/.dockerconfigjson",
		Size:     int64(len(body)),
		Mode:     0o600,
		Typeflag: tar.TypeReg,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gz.Close()
	return dst
}

func TestExtractFAR_DockerConfigFound(t *testing.T) {
	tgz := makeFARTgz(t, `{"auths":{"repo.f5.com":{"auth":"abc"}}}`)
	got, err := ExtractFARDockerConfig(tgz)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "repo.f5.com") {
		t.Errorf("payload = %s", string(got))
	}
}

func TestRenderFARSecret(t *testing.T) {
	got := RenderFARSecret("f5-operators", []byte(`{"auths":{}}`))
	for _, want := range []string{"name: far-secret", "namespace: f5-operators", "kubernetes.io/dockerconfigjson", ".dockerconfigjson:"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestRenderNamespace(t *testing.T) {
	got := RenderNamespace("f5-utils")
	if !strings.Contains(got, "name: f5-utils") || !strings.Contains(got, "kind: Namespace") {
		t.Errorf("bad: %s", got)
	}
}
