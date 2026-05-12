package deploy

import (
	"bytes"
	"fmt"
	"text/template"

	"github.com/mwiget/dpubnkctl/internal/embedded"
)

// FLOInputs are substituted into the embedded FLO values templates.
type FLOInputs struct {
	JWT string // raw JWT (single line)
}

// RenderFLOValues picks the prod or tst template based on jwtType
// and substitutes the JWT.
func RenderFLOValues(jwtType, jwt string) (string, error) {
	tmplName := "templates/flo-values.yaml.tmpl"
	if jwtType == "tst" {
		tmplName = "templates/flo-values-tst.yaml.tmpl"
	}
	raw, err := embedded.Templates.ReadFile(tmplName)
	if err != nil {
		return "", fmt.Errorf("load %s: %w", tmplName, err)
	}
	tmpl, err := template.New("flo").Parse(string(raw))
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	if err := tmpl.Execute(&out, FLOInputs{JWT: jwt}); err != nil {
		return "", err
	}
	return out.String(), nil
}

// CertIssuerChain returns the YAML for the bnk-ca cert-issuer chain
// FLO references as global.certmgr.clusterIssuer = bnk-ca-cluster-issuer:
//
//   1. ClusterIssuer/selfsigned-bnk        (selfsigned root)
//   2. Certificate/bnk-ca in cert-manager  (CA cert + key, signed by selfsigned-bnk)
//   3. ClusterIssuer/bnk-ca-cluster-issuer (uses bnk-ca secret as the CA)
//
// All three resources go to the cert-manager namespace per cert-manager's
// "namespace where ClusterIssuer's secret lives" convention (set by
// cert-manager's --cluster-resource-namespace flag, which defaults to
// cert-manager).
func CertIssuerChain() string {
	return `apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: selfsigned-bnk
spec:
  selfSigned: {}
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: bnk-ca
  namespace: cert-manager
spec:
  isCA: true
  commonName: bnk-ca
  secretName: bnk-ca-secret
  duration: 87600h
  privateKey:
    algorithm: ECDSA
    size: 256
  issuerRef:
    name: selfsigned-bnk
    kind: ClusterIssuer
    group: cert-manager.io
---
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: bnk-ca-cluster-issuer
spec:
  ca:
    secretName: bnk-ca-secret
`
}
