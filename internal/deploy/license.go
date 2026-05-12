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
	Type   string // "prod" | "tst" — from the token's `tst` claim or other heuristic
	Header map[string]any
	Claims map[string]any
}

// InspectJWT base64-decodes the JWT header + claims and classifies it.
// We do NOT verify the signature — this is a content sniff, not auth.
//
// Heuristic for prod vs tst (matches the f5-bnk repo's convention):
//   - if claims contain "tst": true → tst
//   - if header.kid contains "tst" or claims.iss contains "tst" → tst
//   - otherwise → prod
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

	if v, ok := claims["tst"]; ok {
		switch t := v.(type) {
		case bool:
			if t {
				info.Type = "tst"
			}
		case string:
			if strings.EqualFold(t, "true") || strings.EqualFold(t, "tst") {
				info.Type = "tst"
			}
		}
	}
	if iss, ok := claims["iss"].(string); ok && strings.Contains(strings.ToLower(iss), "tst") {
		info.Type = "tst"
	}
	if kid, ok := hdr["kid"].(string); ok && strings.Contains(strings.ToLower(kid), "tst") {
		info.Type = "tst"
	}

	return info, nil
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

// ExtractFARDockerConfig opens the FAR tgz and returns the bytes of the
// dockerconfigjson it contains. The tgz packed by F5 typically holds
// exactly one file named ".dockerconfigjson" (sometimes inside a
// directory) — we return the first match.
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
		base := hdr.Name
		if i := strings.LastIndexByte(base, '/'); i >= 0 {
			base = base[i+1:]
		}
		if base == ".dockerconfigjson" || base == "dockerconfigjson" || base == "config.json" {
			return io.ReadAll(t)
		}
	}
	return nil, fmt.Errorf("far tgz %s: no .dockerconfigjson found", tgzPath)
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
