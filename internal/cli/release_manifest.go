package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/mwiget/dpubnkctl/internal/deploy"
	"github.com/mwiget/dpubnkctl/internal/poc"
	"github.com/mwiget/dpubnkctl/internal/version"
)

// newReleaseManifestCmd assembles `dpubnkctl release-manifest <verb>`.
// The point of exposing it as a top-level subcommand (not just internal
// machinery of `deploy flo`) is so operators can inspect the resolved
// chart and image versions before any cluster write happens.
func newReleaseManifestCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "release-manifest",
		Short: "Inspect the F5 release-manifest chart that pins BNK chart and image versions",
	}
	cmd.AddCommand(newReleaseManifestPullCmd())
	return cmd
}

type releaseManifestPullFlags struct {
	pocDir    string
	version   string
	cacheDir  string
	verbose   bool
	dev       bool
}

func newReleaseManifestPullCmd() *cobra.Command {
	f := &releaseManifestPullFlags{}
	cmd := &cobra.Command{
		Use:   "pull",
		Short: "Pull and parse the BNK release-manifest chart from repo.f5.com",
		Long: `Pull the f5-bigip-k8s-manifest chart from F5's OCI registry, extract it,
and print the resolved chart and image versions for this BNK release.

Authentication uses the FAR service-account key referenced by
poc.yaml.bnk.far_key_ref (defaults to keys/f5-far-auth-key.tgz).

The pulled tgz and extracted YAML are cached under
<poc>/artifacts/release-manifest/ so subsequent deploy steps can reuse
them without re-authenticating to the registry.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runReleaseManifestPull(cmd.Context(), cmd.OutOrStdout(), f)
		},
	}
	cmd.Flags().StringVar(&f.pocDir, "poc", "", "PoC repo path (default: current directory)")
	cmd.Flags().StringVar(&f.version, "version", "",
			"release-manifest version (default: binary-pinned "+version.GetCNEManifestVersion()+")")
	cmd.Flags().StringVar(&f.cacheDir, "cache", "",
		"cache directory (default: <poc>/artifacts/release-manifest)")
	cmd.Flags().BoolVarP(&f.verbose, "verbose", "v", false, "list every chart + image in the manifest")
	cmd.Flags().BoolVar(&f.dev, "dev", false, "Use devrepo.f5.com instead of repo.f5.com for all F5 registry/chart references")
	return cmd
}

func runReleaseManifestPull(ctx context.Context, out io.Writer, f *releaseManifestPullFlags) error {
	version.SetDevRepo(f.dev)
	repo, err := resolvePoCDir(f.pocDir)
	if err != nil {
		return err
	}
	p, err := poc.Load(repo)
	if err != nil {
		return fmt.Errorf("not a PoC repo (%s): %w", repo, err)
	}
	farPath := resolveRef(repo, p.BNK.FARKeyRef)
	if _, err := os.Stat(farPath); err != nil {
		return fmt.Errorf("FAR not found at %s — drop the file there and retry", farPath)
	}
	auth, err := deploy.ExtractFARRegistryAuth(farPath)
	if err != nil {
		return fmt.Errorf("extract FAR registry credentials: %w", err)
	}

	cache := f.cacheDir
	if cache == "" {
		cache = filepath.Join(repo, "artifacts", "release-manifest")
	}
	target := f.version
	if target == "" {
			target = version.GetCNEManifestVersion()
	}

	fmt.Fprintf(out, "Pulling release-manifest %s from %s ...\n", target, version.GetReleaseManifestRepo())
	fmt.Fprintf(out, "  cache:  %s\n\n", cache)

	m, err := deploy.PullReleaseManifest(ctx, auth, target, cache)
	if err != nil {
		return err
	}
	m.SinkSummary(out)

	if f.verbose {
		fmt.Fprintln(out, "\nAll helm charts:")
		for _, name := range sortedKeys(m.HelmCharts) {
			fmt.Fprintf(out, "  %-50s %s\n", name, m.HelmCharts[name])
		}
		fmt.Fprintln(out, "\nAll docker images:")
		for _, name := range sortedKeys(m.DockerImgs) {
			fmt.Fprintf(out, "  %-50s %s\n", name, m.DockerImgs[name])
		}
	}

	fmt.Fprintf(out, "\nResolved manifest cached at %s/manifest.yaml\n",
		cache)
	return nil
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// stdlib sort, but avoid pulling fmt/sort in the file header by
	// inlining a tiny insertion sort — list is small (~30 entries) and
	// this keeps the imports tight.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
