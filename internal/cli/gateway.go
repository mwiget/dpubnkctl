package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

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
