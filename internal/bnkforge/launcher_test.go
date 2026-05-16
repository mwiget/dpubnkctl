package bnkforge

import "testing"

// TestIsLoopbackURL — the InsecureSkipVerify exemption only fires for
// loopback URLs (review S-M3). A remote bnk-forge listener must use
// real TLS so the admin login + kubeconfig POST aren't MITM'd.
func TestIsLoopbackURL(t *testing.T) {
	cases := []struct {
		url  string
		want bool
	}{
		{"https://localhost", true},
		{"https://localhost:8443", true},
		{"http://127.0.0.1", true},
		{"https://127.0.0.1:8443/api", true},
		{"https://[::1]", true},
		{"https://[::1]:8443", true},
		{"https://Localhost", true}, // case-insensitive
		{"https://bnk-forge.lab.f5.com", false},
		{"https://192.168.68.10", false},
		{"https://10.0.0.1", false},
		{"not a url", false},
		{"", false},
	}
	for _, tc := range cases {
		got := isLoopbackURL(tc.url)
		if got != tc.want {
			t.Errorf("isLoopbackURL(%q) = %v, want %v", tc.url, got, tc.want)
		}
	}
}
