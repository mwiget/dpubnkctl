package deploy

import (
	"strings"
	"testing"
)

func TestRenderLicenseCR_Defaults(t *testing.T) {
	got, err := RenderLicenseCR(LicenseInputs{JWT: "abc.def.ghi"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"apiVersion: k8s.f5net.com/v1",
		"kind: License",
		"name: " + LicenseCRName,
		"namespace: " + SharedComponentNamespace,
		`operationMode: "connected"`,
		`jwt: "abc.def.ghi"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q\n--- got ---\n%s", want, got)
		}
	}
}

func TestRenderLicenseCR_Overrides(t *testing.T) {
	got, err := RenderLicenseCR(LicenseInputs{
		Name:          "lab-license",
		Namespace:     "f5-utils",
		OperationMode: "disconnected",
		JWT:           "header.body.sig",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"name: lab-license",
		"namespace: f5-utils",
		`operationMode: "disconnected"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestRenderLicenseCR_RejectsMultilineJWT(t *testing.T) {
	_, err := RenderLicenseCR(LicenseInputs{JWT: "abc\ndef\nghi"})
	if err == nil {
		t.Fatal("expected error for multi-line JWT")
	}
	if !strings.Contains(err.Error(), "newlines") {
		t.Errorf("error should mention newlines, got %v", err)
	}
}

