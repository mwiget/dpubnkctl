package deploy

import (
	"strings"
	"testing"
)

func TestRenderFLOValues_ProdInjectsJWT(t *testing.T) {
	out, err := RenderFLOValues("prod", "EYJ.test.token")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `jwt: "EYJ.test.token"`) {
		t.Errorf("JWT not substituted in prod values")
	}
	if !strings.Contains(out, "product.apis.f5.com") {
		t.Errorf("expected prod TEEM URL in prod values")
	}
	if strings.Contains(out, "product-tst") {
		t.Errorf("prod values leaked tst URLs")
	}
}

func TestRenderFLOValues_TstSwitchesURLs(t *testing.T) {
	out, err := RenderFLOValues("tst", "EYJ.tst.token")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "product-tst.apis.f5networks.net") {
		t.Errorf("expected tst TEEM URL")
	}
	if !strings.Contains(out, `jwt: "EYJ.tst.token"`) {
		t.Errorf("tst JWT not substituted")
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
