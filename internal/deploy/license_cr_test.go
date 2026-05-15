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

func TestInjectNamespace_AddsWhenAbsent(t *testing.T) {
	in := []byte(`apiVersion: v1
kind: Secret
metadata:
  name: cwc-license-certs
type: kubernetes.io/tls
data:
  tls.crt: AAA
`)
	got := injectNamespace(in, "f5-cne-core")
	if !strings.Contains(string(got), "namespace: f5-cne-core") {
		t.Errorf("namespace not injected:\n%s", got)
	}
}

func TestInjectNamespace_PreservesWhenPresent(t *testing.T) {
	in := []byte(`apiVersion: v1
kind: Secret
metadata:
  name: cwc-license-certs
  namespace: f5-utils
type: kubernetes.io/tls
data: {}
`)
	got := string(injectNamespace(in, "f5-cne-core"))
	if strings.Count(got, "namespace:") != 1 {
		t.Errorf("expected exactly one namespace line, got:\n%s", got)
	}
	if !strings.Contains(got, "namespace: f5-utils") {
		t.Errorf("existing namespace overwritten:\n%s", got)
	}
}

func TestInjectNamespace_MultiDoc(t *testing.T) {
	in := []byte(`apiVersion: v1
kind: Secret
metadata:
  name: a
data: {}
---
apiVersion: v1
kind: Secret
metadata:
  name: b
data: {}
`)
	got := string(injectNamespace(in, "f5-cne-core"))
	if strings.Count(got, "namespace: f5-cne-core") != 2 {
		t.Errorf("expected 2 namespace injections, got:\n%s", got)
	}
}
