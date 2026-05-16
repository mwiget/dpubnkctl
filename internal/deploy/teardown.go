package deploy

// BNK sub-CR taxonomy used by `destroy bnk` to force-delete + strip
// finalizers off every custom resource that FLO + the shared-component
// stack create on a healthy install. Lives in the deploy package so
// adding a new BNK 2.x sub-resource means editing the package that
// owns the deploy story, not the CLI file that calls into it.
//
// Each list is "best effort" — missing CRDs no-op under
// --ignore-not-found, so it's safe to leave stale entries here while
// the schema evolves.

// FLOSubCRsInOperators are the F5-group sub-resources that FLO installs
// into the f5-operators namespace on `deploy flo` / `deploy cne`.
// Updated 2026-05-16 against BNK 2.3.0.
var FLOSubCRsInOperators = []string{
	"csrcs",
	"cwcs",
	"observers",
	"rabbitmqs",
	"otelcollectors",
	"cnemanifests",
	"crdinstallers",
	"afms",
	"downloaders",
	"dssms",
	"ipams",
	"envdiscoveries",
	"dwblds",
	"coremonds",
	"analyzers",
	"cnecontrollers",
}

// SubCR identifies a single namespaced custom resource by its plural
// name and API group — together they form the {plural}.{group} string
// kubectl wants for unambiguous addressing (`csrcs.k8s.f5.com` vs.
// `csrcs.spp.example.com`, if such a thing ever existed).
type SubCR struct {
	Plural string
	Group  string
}

// FullName returns "<plural>.<group>" — the kubectl-addressable form.
func (s SubCR) FullName() string { return s.Plural + "." + s.Group }

// SharedComponentSubCRs are the CRs that live in
// SharedComponentNamespace (f5-cne-core) and need their finalizers
// stripped during teardown. The License CR lives in the k8s.f5net.com
// group (separate from the k8s.f5.com sub-resources FLO installs) and
// has its own finalizer chain managed by CWC; by the time destroy bnk
// reaches this step CWC is itself terminating so its watch can't
// clear the finalizer — we strip manually.
//
// Updated 2026-05-16 against BNK 2.3.0.
var SharedComponentSubCRs = []SubCR{
	{"licenses", "k8s.f5net.com"},
	{"cwcs", "k8s.f5.com"},
	{"observers", "k8s.f5.com"},
	{"otelcollectors", "k8s.f5.com"},
	{"rabbitmqs", "k8s.f5.com"},
}
