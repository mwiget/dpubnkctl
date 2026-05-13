package deploy

import (
	"archive/tar"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// JWTInfo describes the parsed JWT we use to pick prod vs tst FLO values.
type JWTInfo struct {
	Type   string // "prod" | "tst"
	JKU    string // header.jku — the JWKS URL that signed this token; primary detection signal
	Sub    string // claims.sub — TST-* prefix is secondary detection signal
	Header map[string]any
	Claims map[string]any
}

// InspectJWT base64-decodes the JWT header + claims and classifies it
// as prod or tst so the right FLO values template (with matching TEEM
// URLs + RSA modulus + x5c chain) is selected.
//
// We do NOT verify the signature — this is a content sniff, not auth.
// Server-side revocation is the real validity check (see AGENTS.md #16).
//
// Detection priority (see AGENTS.md #15):
//
//  1. header.jku — the JWKS URL the token was signed against. This is
//     authoritative because each environment (prod / tst / future stg)
//     publishes its own keys and only verifies tokens signed by them.
//     Substring matches on the hostname:
//       product-tst.apis.f5networks.net → tst
//       product.apis.f5.com             → prod
//
//  2. claims.sub prefix — "TST-*" is a strong secondary signal when
//     jku is missing or unrecognized (e.g. hand-crafted test tokens).
//
//  3. claims.tst (bool / "true") — synthetic fallback only.
//
// We deliberately do NOT match on `iss` or `kid` substring. Real tst
// tokens carry iss="F5 Inc." and kid="v1" (identical to prod), so those
// heuristics fire wrong both ways.
func InspectJWT(jwtPath string) (*JWTInfo, error) {
	data, err := os.ReadFile(jwtPath)
	if err != nil {
		return nil, fmt.Errorf("read jwt %s: %w", jwtPath, err)
	}
	token := strings.TrimSpace(string(data))
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil, fmt.Errorf("jwt %s: not a JWT (need at least header.payload, got %d parts)", jwtPath, len(parts))
	}

	hdr, err := decodeJWTSegment(parts[0])
	if err != nil {
		return nil, fmt.Errorf("decode jwt header: %w", err)
	}
	claims, err := decodeJWTSegment(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode jwt claims: %w", err)
	}

	info := &JWTInfo{Header: hdr, Claims: claims, Type: "prod"}
	if jku, ok := hdr["jku"].(string); ok {
		info.JKU = jku
	}
	if sub, ok := claims["sub"].(string); ok {
		info.Sub = sub
	}

	info.Type = classifyJWT(info.JKU, info.Sub, claims)
	return info, nil
}

// classifyJWT picks "prod" or "tst" from the jku URL (primary), sub
// prefix (secondary), or a synthetic `tst` claim (fallback). Default
// when nothing matches: "prod".
func classifyJWT(jku, sub string, claims map[string]any) string {
	// Primary: hostname inside jku.
	jl := strings.ToLower(jku)
	switch {
	case strings.Contains(jl, "product-tst.apis.f5networks.net"),
		strings.Contains(jl, "product-s-tst.apis.f5networks.net"):
		return "tst"
	case strings.Contains(jl, "product.apis.f5.com"),
		strings.Contains(jl, "product-s.apis.f5.com"):
		return "prod"
	}

	// Secondary: sub prefix.
	if strings.HasPrefix(strings.ToUpper(sub), "TST-") {
		return "tst"
	}

	// Fallback: synthetic `tst` claim (used by test fixtures).
	if v, ok := claims["tst"]; ok {
		switch t := v.(type) {
		case bool:
			if t {
				return "tst"
			}
		case string:
			if strings.EqualFold(t, "true") || strings.EqualFold(t, "tst") {
				return "tst"
			}
		}
	}
	return "prod"
}

func decodeJWTSegment(s string) (map[string]any, error) {
	// JWT uses URL-safe base64 without padding; pad before decoding.
	if pad := len(s) % 4; pad != 0 {
		s += strings.Repeat("=", 4-pad)
	}
	raw, err := base64.URLEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ExtractFARDockerConfig opens the FAR tgz and returns the
// dockerconfigjson bytes ready to drop into a Secret's data field.
//
// Two on-disk formats are supported:
//
//  1. A literal `.dockerconfigjson` (older F5 packs).
//  2. A base64-encoded GCP service-account JSON (current F5 format —
//     filename is typically `cne_pull_64.json`). We decode it,
//     wrap it in the GAR `_json_key:<sa-json>` auth scheme, and build
//     the dockerconfigjson around `repo.f5.com`. Mirrors the recipe in
//     bnk-forge backend/routes/project_secrets.py.
func ExtractFARDockerConfig(tgzPath string) ([]byte, error) {
	f, err := os.Open(tgzPath)
	if err != nil {
		return nil, fmt.Errorf("open far %s: %w", tgzPath, err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("gzip %s: %w", tgzPath, err)
	}
	defer gz.Close()

	t := tar.NewReader(gz)
	for {
		hdr, err := t.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("tar read %s: %w", tgzPath, err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		body, err := io.ReadAll(t)
		if err != nil {
			return nil, err
		}
		base := hdr.Name
		if i := strings.LastIndexByte(base, '/'); i >= 0 {
			base = base[i+1:]
		}

		// Format 1: already a dockerconfigjson.
		if hasAuthsKey(body) {
			return body, nil
		}

		// Format 2: base64-encoded GCP SA JSON. Try base64-decode, then
		// require the decoded blob to be a service_account JSON.
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(body)))
		if err == nil && isServiceAccountJSON(decoded) {
			return buildGARDockerConfig(decoded), nil
		}
		// Or the file may already be raw service-account JSON (no base64 wrapper).
		if isServiceAccountJSON(body) {
			return buildGARDockerConfig(body), nil
		}

		return nil, fmt.Errorf("far tgz %s entry %s is neither a dockerconfigjson nor a (base64-encoded) GCP service_account JSON", tgzPath, base)
	}
	return nil, fmt.Errorf("far tgz %s: no regular files inside", tgzPath)
}

func hasAuthsKey(b []byte) bool {
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return false
	}
	_, ok := m["auths"]
	return ok
}

func isServiceAccountJSON(b []byte) bool {
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return false
	}
	t, _ := m["type"].(string)
	return t == "service_account"
}

// buildGARDockerConfig wraps a GCP service-account JSON as a
// dockerconfigjson for repo.f5.com (GAR-backed).
//
// Auth scheme: "_json_key_base64:<base64-of-sa-json>" base64-encoded.
// Matches f5-bnk-nvidia-bf3-installations v2.2.0-static/extra_playbooks/
// bnk.yml — which is what FLO's manifest-registry code path expects.
//
// (Google Artifact Registry also accepts "_json_key:<raw-sa-json>" for
// docker/helm CLI auth; but FLO's own auth parser only accepts the
// _json_key_base64 form. Bug-for-bug compatibility with f5-bnk wins.)
func buildGARDockerConfig(saJSON []byte) []byte {
	saB64 := base64.StdEncoding.EncodeToString(saJSON)
	auth := base64.StdEncoding.EncodeToString([]byte("_json_key_base64:" + saB64))
	cfg := fmt.Sprintf(`{"auths":{"repo.f5.com":{"auth":%q}}}`, auth)
	return []byte(cfg)
}

// UnwrapGARAuth is the inverse of buildGARDockerConfig: given a
// dockerconfigjson with auths.repo.f5.com.auth, returns the raw
// service-account JSON. Used by `helm registry login` which needs the
// password directly. Handles both auth forms in case an operator
// supplies a hand-crafted dockerconfigjson:
//
//   _json_key:<raw-json>            (older bnk-forge convention)
//   _json_key_base64:<base64-json>  (current f5-bnk convention)
func UnwrapGARAuth(dockerCfg []byte) (string, error) {
	var cfg struct {
		Auths map[string]struct {
			Auth string `json:"auth"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(dockerCfg, &cfg); err != nil {
		return "", err
	}
	entry, ok := cfg.Auths["repo.f5.com"]
	if !ok {
		return "", fmt.Errorf("dockerconfigjson has no auths.repo.f5.com")
	}
	raw, err := base64.StdEncoding.DecodeString(entry.Auth)
	if err != nil {
		return "", fmt.Errorf("decode auth: %w", err)
	}
	s := string(raw)
	switch {
	case strings.HasPrefix(s, "_json_key_base64:"):
		// password is base64-encoded SA JSON; decode it back.
		pw := s[len("_json_key_base64:"):]
		dec, err := base64.StdEncoding.DecodeString(pw)
		if err != nil {
			return "", fmt.Errorf("decode _json_key_base64 password: %w", err)
		}
		return string(dec), nil
	case strings.HasPrefix(s, "_json_key:"):
		return s[len("_json_key:"):], nil
	default:
		return "", fmt.Errorf("auth does not start with _json_key: or _json_key_base64:")
	}
}

// RenderFARSecret produces a Secret manifest of type
// kubernetes.io/dockerconfigjson named "far-secret" in the given
// namespace. dockerConfigJSON is raw JSON (not base64-encoded — k8s
// requires base64 in `data:`, so we encode here).
func RenderFARSecret(namespace string, dockerConfigJSON []byte) string {
	encoded := base64.StdEncoding.EncodeToString(dockerConfigJSON)
	return fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: far-secret
  namespace: %s
type: kubernetes.io/dockerconfigjson
data:
  .dockerconfigjson: %s
`, namespace, encoded)
}

// RenderNamespace returns a minimal Namespace manifest.
func RenderNamespace(name string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Namespace
metadata:
  name: %s
`, name)
}
