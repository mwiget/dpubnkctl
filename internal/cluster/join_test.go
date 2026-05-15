package cluster

import "testing"

func TestNormalizeK8sMinor(t *testing.T) {
	cases := []struct{ in, want string }{
		{"1.30", "1.30"},
		{"1.30.14", "1.30"},
		{"v1.30.14", "1.30"},
		{"  1.30.14  ", "1.30"},
		{"1.35.3", "1.35"},
		// Garbage in stays garbage out; the apt URL will fail loudly later.
		{"1", "1"},
		{"", ""},
	}
	for _, c := range cases {
		got := normalizeK8sMinor(c.in)
		if got != c.want {
			t.Errorf("normalizeK8sMinor(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
