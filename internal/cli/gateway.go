package cli

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mwiget/dpubnkctl/internal/deploy"
	"github.com/mwiget/dpubnkctl/internal/poc"
)

func newGatewayCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "gateway",
		Short: "Scaffold Gateway / HTTPRoute manifests against the BNK GatewayClass",
		Long: `BNK 2.2.0's f5-cne-controller has no global IPAM pool — every
Gateway must declare an explicit spec.addresses value. Hand-editing
that on every test app gets old fast, especially for agentic flows.

These subcommands print ready-to-apply YAML using poc.yaml's
bnk.external_selfip (or --address override) so the operator can just
'dpubnkctl gateway example | kubectl apply -f -'.

When BNK adds a real default-pool mechanism we'll grow a 'deploy
ipam-pool' subcommand alongside this one. For now: explicit per-Gateway
addresses are the only supported path.`,
	}
	cmd.AddCommand(newGatewayExampleCmd())
	cmd.AddCommand(newGatewayResyncCmd())
	return cmd
}

type gatewayExampleFlags struct {
	pocDir    string
	name      string
	port      int
	address   string
	appName   string
	appImage  string
	appPort   int
	smokeTest bool
}

func newGatewayExampleCmd() *cobra.Command {
	f := &gatewayExampleFlags{}
	cmd := &cobra.Command{
		Use:   "example",
		Short: "Print a Gateway + HTTPRoute YAML targeting bnk-gatewayclass",
		Long: `Render a working Gateway + HTTPRoute scaffold to stdout, ready to pipe
into kubectl apply. spec.addresses is filled in from one of:

  1. --address <ip>                       (explicit override)
  2. poc.yaml bnk.external_selfip         (default)

Add --smoke-test to also emit a backend Deployment + Service so the
operator can exercise the full path end-to-end without writing any YAML
by hand. The smoke test matches the shape in homelab/journal/
2026-05-14-smoke-test.md (nginx behind the Gateway).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runGatewayExample(cmd.OutOrStdout(), f)
		},
	}
	cmd.Flags().StringVar(&f.pocDir, "poc", "", "PoC repo path (default: current directory)")
	cmd.Flags().StringVar(&f.name, "name", "demo-gw", "Gateway / HTTPRoute name")
	cmd.Flags().IntVar(&f.port, "port", 80, "Gateway listener port")
	cmd.Flags().StringVar(&f.address, "address", "", "Override spec.addresses (defaults to poc.yaml bnk.external_selfip)")
	cmd.Flags().StringVar(&f.appName, "app-name", "demo-app", "Backend Service/Deployment name (smoke-test only)")
	cmd.Flags().StringVar(&f.appImage, "app-image", "nginx:1.27-alpine", "Backend container image (smoke-test only)")
	cmd.Flags().IntVar(&f.appPort, "app-port", 80, "Backend container/service port (smoke-test only)")
	cmd.Flags().BoolVar(&f.smokeTest, "smoke-test", false, "Also emit a Deployment + Service so the operator can curl through the Gateway end-to-end")
	return cmd
}

func runGatewayExample(out io.Writer, f *gatewayExampleFlags) error {
	repo, err := resolvePoCDir(f.pocDir)
	if err != nil {
		return err
	}
	p, err := poc.Load(repo)
	if err != nil {
		return fmt.Errorf("not a PoC repo (%s): %w", repo, err)
	}
	addr := strings.TrimSpace(f.address)
	if addr == "" {
		addr = strings.TrimSpace(p.BNK.ExternalSelfIP)
	}
	if addr == "" {
		return fmt.Errorf("no Gateway address: pass --address <ip> or set bnk.external_selfip in poc.yaml")
	}

	var b strings.Builder
	if f.smokeTest {
		renderSmokeBackend(&b, f.appName, f.appImage, f.appPort)
		b.WriteString("---\n")
	}
	renderGatewayHTTPRoute(&b, f.name, addr, f.port, f.appName, f.appPort, f.smokeTest)
	_, err = io.WriteString(out, b.String())
	return err
}

// renderSmokeBackend emits a minimal Deployment + Service so the operator
// has something to point the HTTPRoute at without writing app YAML. Single
// replica is intentional — this is a smoke test, not an HA test.
func renderSmokeBackend(b *strings.Builder, name, image string, port int) {
	fmt.Fprintf(b, `apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
spec:
  replicas: 1
  selector:
    matchLabels: { app: %s }
  template:
    metadata:
      labels: { app: %s }
    spec:
      containers:
        - name: %s
          image: %s
          ports:
            - containerPort: %d
---
apiVersion: v1
kind: Service
metadata:
  name: %s
spec:
  selector: { app: %s }
  ports:
    - port: %d
      targetPort: %d
`, name, name, name, name, image, port, name, name, port, port)
}

// renderGatewayHTTPRoute emits the BNK-targeted Gateway and a matching
// HTTPRoute. spec.addresses is mandatory in BNK 2.2.0 (no global IPAM
// pool); without it the Gateway sits at Programmed=False with
// reason=AddressNotAssigned (homelab journal entry 2026-05-14-smoke-test).
func renderGatewayHTTPRoute(b *strings.Builder, name, address string, port int, backendName string, backendPort int, withRoute bool) {
	fmt.Fprintf(b, `apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: %s
spec:
  gatewayClassName: bnk-gatewayclass
  addresses:
    # BNK 2.2.0's f5-cne-controller has no global IPAM pool; every
    # Gateway must declare its IP explicitly or stay Programmed=False.
    - type: IPAddress
      value: %s
  listeners:
    - name: http
      port: %d
      protocol: HTTP
      allowedRoutes:
        namespaces:
          from: Same
`, name, address, port)
	if withRoute {
		// BNK 2.3 tightened Gateway API conformance: an HTTPRoute with
		// no `hostnames:` no longer matches catch-all traffic — TMM
		// falls through to the "no virtual server" fallback and emits
		// HTTP/1.0 500 "Server: BigIP" instead of routing to the
		// backend. Always include at least one hostname; operators can
		// curl with -H "Host: <hostname>" or set up DNS pointing at
		// the gateway's spec.addresses value. See AGENTS.md gotcha #27.
		fmt.Fprintf(b, `---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: %s
spec:
  parentRefs:
    - name: %s
  # Required on BNK 2.3 — without a hostname TMM returns HTTP 500.
  # Curl with: -H "Host: %s.local"
  hostnames:
    - "%s.local"
  rules:
    - matches:
        - path:
            type: PathPrefix
            value: /
      backendRefs:
        - name: %s
          port: %d
`, name, name, backendName, backendName, backendName, backendPort)
	}
}

// ---------------------------------------------------------------------------
// `dpubnkctl gateway resync`
//
// Workaround for AGENTS.md #28: BNK 2.3's f5-cne-controller pushes virtual-
// server config to TMM pods on Gateway / HTTPRoute create/update events
// only — it does NOT push to a TMM that joins the cluster later. A late-
// joining TMM ends up with zero virtual servers programmed; ~50% of LACP-
// hashed flows hit the BIG-IP "no virtual server" fallback (HTTP/1.0 500).
//
// The known recovery: delete and re-apply the Gateway + HTTPRoute pair so
// the controller emits Update events and re-pushes to every TMM currently
// present. This subcommand automates that on every Gateway in the cluster
// that targets bnk-gatewayclass.
//
// See docs/upstream/f5-cne-controller-tmm-resync-on-join.md for the
// upstream issue write-up.
// ---------------------------------------------------------------------------

type gatewayResyncFlags struct {
	pocDir    string
	namespace string
	dryRun    bool
}

func newGatewayResyncCmd() *cobra.Command {
	f := &gatewayResyncFlags{}
	cmd := &cobra.Command{
		Use:   "resync",
		Short: "Force the cne-controller to re-push every BNK Gateway to all TMMs (AGENTS.md #28)",
		Long: `Walk every Gateway in the cluster that targets gatewayClassName=
bnk-gatewayclass, capture its live spec, delete it (and any HTTPRoutes
referencing it), wait briefly for the cne-controller to observe the
delete, then re-apply. The Update events the controller emits cause
it to push the virtual-server config to every TMM that is currently
in the cluster — including TMMs that joined AFTER the Gateway was
originally applied.

Use this after any of:
  - a TMM pod crashed + restarted
  - a new DPU node was added post-deploy
  - the multus first-start CNI race delayed a TMM
  - any FLO rolling update that cycled TMMs

It's a workaround. The right fix is in f5-cne-controller; see
docs/upstream/f5-cne-controller-tmm-resync-on-join.md.

There is a brief gap (~3-8s per Gateway) during which the Gateway
exists with Programmed=False. Existing client connections may break
in that window. Don't run during production traffic.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runGatewayResync(cmd.Context(), cmd.OutOrStdout(), f)
		},
	}
	cmd.Flags().StringVar(&f.pocDir, "poc", "", "PoC repo path (default: current directory)")
	cmd.Flags().StringVar(&f.namespace, "namespace", "", "Limit to one namespace (default: all)")
	cmd.Flags().BoolVar(&f.dryRun, "dry-run", false, "Print what would be resynced; do not modify the cluster")
	return cmd
}

func runGatewayResync(ctx context.Context, out io.Writer, f *gatewayResyncFlags) error {
	repo, err := resolvePoCDir(f.pocDir)
	if err != nil {
		return err
	}
	if _, err := poc.Load(repo); err != nil {
		return fmt.Errorf("not a PoC repo (%s): %w", repo, err)
	}
	kubeconfig := filepath.Join(repo, "artifacts", "kubeconfig")
	r := &deploy.Runner{KubeconfigPath: kubeconfig, Out: prefixWriter{w: out, prefix: "      | "}}

	// List Gateways. -A unless a single namespace was passed.
	listArgs := []string{"get", "gateway",
		"-o", `jsonpath={range .items[*]}{.metadata.namespace}{"\t"}{.metadata.name}{"\t"}{.spec.gatewayClassName}{"\n"}{end}`}
	if f.namespace != "" {
		listArgs = append([]string{"-n", f.namespace}, listArgs...)
	} else {
		listArgs = append(listArgs, "-A")
	}
	raw, err := r.KubectlCapture(ctx, listArgs...)
	if err != nil {
		return fmt.Errorf("list gateways: %w", err)
	}

	type gw struct{ ns, name string }
	var targets []gw
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) < 3 {
			continue
		}
		if fields[2] != "bnk-gatewayclass" {
			continue
		}
		targets = append(targets, gw{fields[0], fields[1]})
	}

	if len(targets) == 0 {
		fmt.Fprintln(out, "No bnk-gatewayclass Gateways found. Nothing to resync.")
		return nil
	}
	fmt.Fprintf(out, "Found %d bnk-gatewayclass Gateway(s):\n", len(targets))
	for _, g := range targets {
		fmt.Fprintf(out, "  - %s/%s\n", g.ns, g.name)
	}
	if f.dryRun {
		fmt.Fprintln(out, "\n(dry-run; no changes applied)")
		return nil
	}

	for i, g := range targets {
		fmt.Fprintf(out, "\n[%d/%d] resync %s/%s\n", i+1, len(targets), g.ns, g.name)

		// Capture the Gateway YAML, stripping status / resourceVersion
		// so the re-apply doesn't conflict with whatever the controller
		// last wrote. `--show-managed-fields=false` keeps the body
		// small; `kubectl-neat`-style cleanup would be nicer but isn't
		// in alpine/k8s.
		gwYAML, err := r.KubectlCapture(ctx, "-n", g.ns, "get", "gateway", g.name, "-o", "yaml")
		if err != nil {
			fmt.Fprintf(out, "      WARN: capture %s/%s: %v — skipping\n", g.ns, g.name, err)
			continue
		}
		gwYAML = stripK8sServerFields(gwYAML)

		// Capture every HTTPRoute that references this Gateway by name.
		// We need them because the HTTPRoute's binding to the Gateway
		// has to be re-established post-delete; without re-applying the
		// route, the new Gateway would have Listener attachedRoutes=0.
		// JSONPath: match parentRefs[].name == <gw>.
		routesRaw, _ := r.KubectlCapture(ctx, "-n", g.ns, "get", "httproute",
			"-o", `jsonpath={range .items[?(@.spec.parentRefs[*].name=="`+g.name+`")]}{.metadata.name}{"\n"}{end}`)
		var routeYAMLs []string
		for _, rn := range strings.Split(strings.TrimSpace(routesRaw), "\n") {
			rn = strings.TrimSpace(rn)
			if rn == "" {
				continue
			}
			rb, err := r.KubectlCapture(ctx, "-n", g.ns, "get", "httproute", rn, "-o", "yaml")
			if err != nil {
				fmt.Fprintf(out, "      WARN: capture httproute %s/%s: %v\n", g.ns, rn, err)
				continue
			}
			routeYAMLs = append(routeYAMLs, stripK8sServerFields(rb))
		}
		fmt.Fprintf(out, "      captured Gateway + %d HTTPRoute(s)\n", len(routeYAMLs))

		// Delete (HTTPRoutes first, then Gateway).
		for _, rn := range strings.Split(strings.TrimSpace(routesRaw), "\n") {
			rn = strings.TrimSpace(rn)
			if rn == "" {
				continue
			}
			_ = r.Kubectl(ctx, "-n", g.ns, "delete", "httproute", rn, "--ignore-not-found")
		}
		_ = r.Kubectl(ctx, "-n", g.ns, "delete", "gateway", g.name, "--ignore-not-found")

		// Brief settle so the controller processes the deletes before
		// the recreate (observed: < 1s; allow 3s for safety).
		time.Sleep(3 * time.Second)

		// Re-apply: Gateway first, then HTTPRoutes.
		if err := r.ApplyInNamespace(ctx, g.ns, gwYAML); err != nil {
			fmt.Fprintf(out, "      WARN: re-apply gateway %s/%s: %v\n", g.ns, g.name, err)
			continue
		}
		for _, rb := range routeYAMLs {
			if err := r.ApplyInNamespace(ctx, g.ns, rb); err != nil {
				fmt.Fprintf(out, "      WARN: re-apply httproute: %v\n", err)
			}
		}
		fmt.Fprintf(out, "      %s/%s resynced.\n", g.ns, g.name)
	}

	fmt.Fprintln(out, "\nDone. Smoke-test traffic now — every TMM in the cluster has the latest config.")
	return nil
}

// stripK8sServerFields removes metadata.resourceVersion, .uid,
// .creationTimestamp, .generation, and .status from a YAML body so the
// document can be `kubectl apply`d cleanly without a Conflict from the
// server-side state. Naive line-oriented strip; relies on the standard
// yaml-formatted output of `kubectl get -o yaml`.
func stripK8sServerFields(yamlBody string) string {
	var out strings.Builder
	inStatus := false
	for _, line := range strings.Split(yamlBody, "\n") {
		// status: starts the block; everything indented under it
		// gets dropped until we see a top-level (non-indented) line.
		if strings.HasPrefix(line, "status:") {
			inStatus = true
			continue
		}
		if inStatus {
			if line == "" || strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
				continue
			}
			inStatus = false
		}
		// metadata fields the server fills in: drop any that appear
		// at exactly 2-space indent under metadata.
		trim := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "  resourceVersion:"),
			strings.HasPrefix(line, "  uid:"),
			strings.HasPrefix(line, "  generation:"),
			strings.HasPrefix(line, "  creationTimestamp:"),
			strings.HasPrefix(line, "  selfLink:"):
			continue
		}
		_ = trim
		out.WriteString(line)
		out.WriteByte('\n')
	}
	return out.String()
}
