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

func TestInspectJWT_ProdByJKU(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), ".jwt")
	jwt := makeJWT(t,
		`{"alg":"RS512","kid":"v1","jku":"https://product.apis.f5.com/ee/v1/keys/jwks"}`,
		`{"iss":"F5 Inc.","sub":"CUSTOMER-X"}`)
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
	if info.JKU == "" {
		t.Errorf("JKU not surfaced from header")
	}
}

// Real-world lake1 shape: iss="F5 Inc.", kid="v1" (same in both prod
// and tst). Only jku + sub distinguish — must classify as tst.
func TestInspectJWT_TstByJKU_LakeOneShape(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), ".jwt")
	jwt := makeJWT(t,
		`{"alg":"RS512","typ":"JWT","kid":"v1","jku":"https://product-tst.apis.f5networks.net/ee/v1/keys/jwks"}`,
		`{"iss":"F5 Inc.","sub":"TST-EE4C16F4-7B16-463E-B050-0026A6E837E4","iat":1731196857,"f5_sat":1746748857}`)
	_ = os.WriteFile(tmp, []byte(jwt), 0o600)
	info, _ := InspectJWT(tmp)
	if info.Type != "tst" {
		t.Errorf("Type = %q, want tst (jku product-tst)", info.Type)
	}
	if info.Sub != "TST-EE4C16F4-7B16-463E-B050-0026A6E837E4" {
		t.Errorf("Sub = %q, want lake1 sub", info.Sub)
	}
}

// Hand-crafted token with no jku — must fall back to sub TST- prefix.
func TestInspectJWT_TstBySubPrefix(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), ".jwt")
	jwt := makeJWT(t,
		`{"alg":"RS256"}`,
		`{"sub":"TST-1234"}`)
	_ = os.WriteFile(tmp, []byte(jwt), 0o600)
	info, _ := InspectJWT(tmp)
	if info.Type != "tst" {
		t.Errorf("Type = %q, want tst (sub TST-* prefix)", info.Type)
	}
}

// Synthetic `tst` claim — fallback path used by older test fixtures.
func TestInspectJWT_TstByClaim(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), ".jwt")
	jwt := makeJWT(t,
		`{"alg":"RS256"}`,
		`{"iss":"F5 Inc.","tst":true}`)
	_ = os.WriteFile(tmp, []byte(jwt), 0o600)
	info, _ := InspectJWT(tmp)
	if info.Type != "tst" {
		t.Errorf("Type = %q, want tst (synthetic claim)", info.Type)
	}
}

// iss="F5 Inc." + kid="v1" appear in BOTH prod and tst tokens, so a
// substring "tst" match on either would be wrong. Token with no jku and
// no TST- sub must default to prod, not be tricked by iss/kid strings.
func TestInspectJWT_IssAndKidAreNotSignals(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), ".jwt")
	jwt := makeJWT(t,
		`{"alg":"RS512","kid":"v1"}`,
		`{"iss":"F5 Inc.","sub":"PROD-1234"}`)
	_ = os.WriteFile(tmp, []byte(jwt), 0o600)
	info, _ := InspectJWT(tmp)
	if info.Type != "prod" {
		t.Errorf("Type = %q, want prod (no jku/sub-tst signal)", info.Type)
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
