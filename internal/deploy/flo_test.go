package deploy

import (
	"strings"
	"testing"
)

func TestRenderFLOValues_Defaults(t *testing.T) {
	out, err := RenderFLOValues(FLOInputs{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"namespace: f5-operators",
		"sharedComponentNamespace: " + SharedComponentNamespace,
		"clusterIssuer: bnk-ca-cluster-issuer",
		"containerPlatform: Generic",
		"name: far-secret",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q\n--- got ---\n%s", want, out)
		}
	}
}

func TestRenderFLOValues_NoLicenseBlock(t *testing.T) {
	out, err := RenderFLOValues(FLOInputs{})
	if err != nil {
		t.Fatal(err)
	}
	// In 2.3 the license + TEEM cert chain MUST NOT be in chart values.
	// Anything resembling those is a regression to the 2.2 shape.
	for _, banned := range []string{
		"jwt:",
		"teemCertUrl",
		"teemEntitlementUrl",
		"product.apis.f5.com",
		"product-tst",
		"licenseserverrootca",
		"modulus:",
	} {
		if strings.Contains(out, banned) {
			t.Errorf("FLO values leaked %q from the 2.2 shape:\n%s", banned, out)
		}
	}
}

func TestRenderFLOValues_HonorsOverrides(t *testing.T) {
	out, err := RenderFLOValues(FLOInputs{
		Namespace:                "f5-bnk",
		SharedComponentNamespace: "f5-utils",
		ClusterIssuer:            "selfsigned-cluster-issuer",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"namespace: f5-bnk",
		"sharedComponentNamespace: f5-utils",
		"clusterIssuer: selfsigned-cluster-issuer",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestCertIssuerChain_ContainsAllThreeResources(t *testing.T) {
	chain := CertIssuerChain()
	for _, want := range []string{
		"name: selfsigned-bnk",
		"name: bnk-ca",
		"name: bnk-ca-cluster-issuer",
		"isCA: true",
		"secretName: bnk-ca-secret",
	} {
		if !strings.Contains(chain, want) {
			t.Errorf("missing %q", want)
		}
	}
}
