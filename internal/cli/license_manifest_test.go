package cli

import "testing"

// licenseManifestPayload is the last gate before the CWC /receipt
// one-shot. Every case below is something that reached the old
// `len(data) >= 100` check and passed it.
func TestLicenseManifestPayload_RejectsNonManifests(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"empty", ""},
		{"whitespace only", "   \n\t "},
		{
			// A 502 from a proxy on the way to product.apis.f5.com.
			"html error page",
			`<html><head><title>502 Bad Gateway</title></head><body>` +
				`<h1>502 Bad Gateway</h1><p>The upstream server is unavailable. ` +
				`Please retry your request in a few moments.</p></body></html>`,
		},
		{
			// F5 rejecting the JWT — valid JSON, over 100 bytes, no manifest.
			"json error response",
			`{"error":"unauthorized","message":"the supplied entitlement token ` +
				`could not be verified against the issuing environment","status":401}`,
		},
		{"manifest present but empty", `{"manifest":"","status":"ok","padding":"` + pad(120) + `"}`},
		{"manifest present but blank", `{"manifest":"   ","padding":"` + pad(120) + `"}`},
		{"truncated json", `{"manifest":"eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJ`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := licenseManifestPayload([]byte(tc.body))
			if err == nil {
				t.Fatalf("accepted a non-manifest (%d bytes) and would have spent the one-shot; returned %q",
					len(tc.body), got)
			}
		})
	}
}

func TestLicenseManifestPayload_ExtractsManifest(t *testing.T) {
	// The receipt endpoint wants the signed manifest itself, not the
	// envelope F5 returns it in.
	const signed = "eyJhbGciOiJSUzI1NiJ9.eyJlbnRpdGxlbWVudCI6InRlc3QifQ.c2ln"
	body := `{"manifest":"` + signed + `","digitalAssetId":"TST-1234","status":"issued"}`

	got, err := licenseManifestPayload([]byte(body))
	if err != nil {
		t.Fatalf("rejected a valid manifest: %v", err)
	}
	if string(got) != signed {
		t.Errorf("payload = %q, want the manifest value %q", got, signed)
	}
}

func TestLicenseManifestPayload_ShortButValid(t *testing.T) {
	// Guards the inverse of the old bug: a legitimate response under the
	// old 100-byte floor must not be rejected for its size.
	got, err := licenseManifestPayload([]byte(`{"manifest":"abc"}`))
	if err != nil {
		t.Fatalf("rejected a short but valid manifest: %v", err)
	}
	if string(got) != "abc" {
		t.Errorf("payload = %q, want %q", got, "abc")
	}
}

func pad(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'x'
	}
	return string(b)
}
