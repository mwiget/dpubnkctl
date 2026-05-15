package deploy

import (
	"bytes"
	"fmt"
	"text/template"

	"github.com/mwiget/dpubnkctl/internal/embedded"
)

// FLOInputs are substituted into the embedded FLO values template.
//
// In 2.3 the prod/tst-specific TEMM cert chain + URLs are GONE — the
// F5 Cluster-Wide Controller (CWC) reads the TEMM endpoint from the
// JWT's `jku` header at runtime, so a single template serves both
// environments. Operators still drop a tst JWT into keys/.jwt; the
// downstream behavior differs without any operator-supplied config.
type FLOInputs struct {
	Namespace                string // FLO release namespace (default f5-operators)
	SharedComponentNamespace string // CWC + license + observer namespace (default f5-cne-core)
	ClusterIssuer            string // cert-manager ClusterIssuer FLO uses for internal CAs
}

// RenderFLOValues substitutes the embedded flo-values.yaml.tmpl. Caller
// fills in any non-default fields; zero values fall back to the F5
// docs canonical names.
func RenderFLOValues(in FLOInputs) (string, error) {
	if in.Namespace == "" {
		in.Namespace = "f5-operators"
	}
	if in.SharedComponentNamespace == "" {
		in.SharedComponentNamespace = SharedComponentNamespace
	}
	if in.ClusterIssuer == "" {
		in.ClusterIssuer = "bnk-ca-cluster-issuer"
	}
	raw, err := embedded.Templates.ReadFile("templates/flo-values.yaml.tmpl")
	if err != nil {
		return "", fmt.Errorf("load flo-values.yaml.tmpl: %w", err)
	}
	tmpl, err := template.New("flo").Parse(string(raw))
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	if err := tmpl.Execute(&out, in); err != nil {
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
