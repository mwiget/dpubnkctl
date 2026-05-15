package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/mwiget/dpubnkctl/internal/version"
)

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version, build, and pinned BNK component versions",
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "dpubnkctl  %s  (commit %s, built %s)\n",
				version.Version, version.Commit, version.BuildDate)
			fmt.Fprintf(out, "Targets    BNK %s\n", version.BNKVersion)
			fmt.Fprintln(out, "Pinned components:")
			fmt.Fprintf(out, "  DOCA / BFB     %s  (%s)\n", version.DOCAVersion, version.BFBImage)
			fmt.Fprintf(out, "  FLO chart      (resolved at deploy time from release-manifest %s)\n", version.CNEManifestVersion)
			fmt.Fprintf(out, "                 %s\n", version.FLOChartOCIRef)
			fmt.Fprintf(out, "  Kubernetes     %s\n", version.K8sVersion)
			fmt.Fprintf(out, "  containerd     %s\n", version.ContainerdVer)
			fmt.Fprintf(out, "  runc           %s\n", version.RuncVersion)
			fmt.Fprintf(out, "  pause image    %s\n", version.PauseImageTag)
			fmt.Fprintln(out)
			fmt.Fprintln(out, topologiesShortSummary)
			return nil
		},
	}
}
