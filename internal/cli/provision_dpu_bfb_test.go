package cli

import (
	"context"
	"io"
	"testing"

	"github.com/mwiget/dpubnkctl/internal/poc"
)

// resolveBFBPlan's push branch downloads via EnsureBFB (network), so these
// tests exercise only the onhost / fetch / error branches, which are pure
// decision logic.

func TestResolveBFBPlan_FlagOverridesPoC(t *testing.T) {
	p := &poc.PoC{}
	p.Provisioning.BFBFetch = poc.BFBFetchPush // PoC says push
	p.Versions.BFBImage = "img.bfb"
	f := &provisionDPUFlags{bfbFetch: poc.BFBFetchHost} // flag says host → wins

	plan, err := resolveBFBPlan(context.Background(), io.Discard, p, f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan.mode != bfbModeFetch {
		t.Errorf("flag should override PoC: mode = %q, want %q", plan.mode, bfbModeFetch)
	}
	if plan.hostPath == "" || plan.fetchURL == "" {
		t.Errorf("fetch plan missing hostPath/fetchURL: %+v", plan)
	}
}

func TestResolveBFBPlan_PoCFetchHost(t *testing.T) {
	p := &poc.PoC{}
	p.Provisioning.BFBFetch = poc.BFBFetchHost
	p.Versions.BFBImage = "img.bfb"
	f := &provisionDPUFlags{}

	plan, err := resolveBFBPlan(context.Background(), io.Discard, p, f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan.mode != bfbModeFetch {
		t.Errorf("mode = %q, want %q", plan.mode, bfbModeFetch)
	}
}

func TestResolveBFBPlan_OnHost(t *testing.T) {
	p := &poc.PoC{}
	p.Provisioning.BFBOnHost = "/var/cache/dpubnkctl/bfb/x.bfb"
	f := &provisionDPUFlags{}

	plan, err := resolveBFBPlan(context.Background(), io.Discard, p, f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan.mode != bfbModeOnHost {
		t.Errorf("mode = %q, want %q", plan.mode, bfbModeOnHost)
	}
	if plan.hostPath != p.Provisioning.BFBOnHost {
		t.Errorf("hostPath = %q, want %q", plan.hostPath, p.Provisioning.BFBOnHost)
	}
}

func TestResolveBFBPlan_MutualExclusion(t *testing.T) {
	p := &poc.PoC{}
	p.Provisioning.BFBOnHost = "/var/cache/dpubnkctl/bfb/x.bfb"
	f := &provisionDPUFlags{bfbFetch: poc.BFBFetchHost} // conflicts with bfb_on_host

	if _, err := resolveBFBPlan(context.Background(), io.Discard, p, f); err == nil {
		t.Error("expected mutual-exclusion error, got nil")
	}
}

func TestResolveBFBPlan_InvalidMode(t *testing.T) {
	p := &poc.PoC{}
	f := &provisionDPUFlags{bfbFetch: "pull"}
	if _, err := resolveBFBPlan(context.Background(), io.Discard, p, f); err == nil {
		t.Error("expected invalid-mode error, got nil")
	}
}

func TestResolveBFBPlan_ExpectedSHAPrecedence(t *testing.T) {
	poced := "4840d8ff1ed3539eac2a1afd04378abda5104ac21f710046dce77274ca9162e4"
	p := &poc.PoC{}
	p.Provisioning.BFBOnHost = "/var/cache/x.bfb"
	p.Provisioning.BFBSHA256 = poced
	f := &provisionDPUFlags{}

	plan, err := resolveBFBPlan(context.Background(), io.Discard, p, f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan.expectedSHA != poced {
		t.Errorf("expectedSHA = %q, want poc override %q", plan.expectedSHA, poced)
	}
	if plan.skipChecksum {
		t.Error("skipChecksum should default false")
	}
}
