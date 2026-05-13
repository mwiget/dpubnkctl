package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// topologiesTable is the supported-topologies reference, embedded in the
// binary so a customer with only the binary can see what shapes it
// handles. Single source of truth — same text shows up in `dpubnkctl
// topologies` (full) and a one-block summary in `dpubnkctl version`.
const topologiesTable = `Supported topologies (BNK 2.2.0):

  Topology               Hosts  CP   Workers              HA-safe?   Use case
  ---------------------- -----  ---  -------------------  ---------  ----------------------------
  Single-node lab        1      1*   1*                   No         Smoke test / laptop demo
  Minimum multi-node     2      1    1 host + DPUs        No         First customer PoC
  Two control planes     2      2*   DPUs (and CPs)       No (note)  Validated on lake1
  Production HA          3+     3    DPUs + opt. hosts    Yes        Long-running deployments

  * means role: both (control-plane + worker on the same host).
  (note) 2 CPs keep etcd quorum on a single failure but a 3rd host is needed
  for true HA. dpubnkctl validate warns on this shape.

Per-topology notes:

  - DPUs always join externally (not via kubespray). Each host declares 1+
    DPUs in poc.yaml.hosts[*].dpus; dpubnkctl cluster join-dpus joins them
    after the kubespray cluster.yml run completes.
  - Each host can be control-plane | worker | both via host.role.
  - Each DPU is LAG or non-LAG. LAG bonds p0+p1 into one fabric uplink
    (LACP on the switch side); non-LAG uses p0 and p1 as two independent
    uplinks with vlans[].uplink: p0|p1.
  - No fixed upper bound on hosts or DPUs. The binary is end-to-end
    validated on the lake1 lab (2 hosts × 1 DPU). Larger clusters are
    syntactically supported but un-soaked.
`

// topologiesExamples are minimal poc.yaml snippets the operator can crib
// from. Kept short — full schema is in internal/poc/schema.go.
const topologiesExamples = `Example poc.yaml shapes:

  # Single-node lab (1 host, role: both, 1 DPU, LAG)
  hosts:
    - name: lab1
      role: both
      ssh: { address: 192.168.68.10, user: ubuntu, key_ref: keys/lab1 }
      data_plane: { parent_iface: ens16f0np0, vlans: [{role: internal, tag: 41, ip: 10.10.41.10/24}] }
      dpus:
        - { pci: "0000:03:00.0", mode: dpu, lag: true, hostname: lab1-bf3,
            tmfifo_ip: 192.168.100.2/30,
            vlans: [{role: internal, tag: 41, ip: 10.10.41.5/24},
                    {role: external, tag: 40, ip: 10.10.40.5/24}] }

  # Two control planes (lake1 shape) — same as above but 2 hosts,
  # both role: both, DPUs join as workers afterwards.

  # Production HA — 3 hosts with role: control-plane (or both) + N DPUs.
  # cluster_apiserver_address points at a VIP on the data-plane fabric
  # so externally-joined DPUs hit a stable endpoint.
`

func newTopologiesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "topologies",
		Short: "Print supported cluster topologies + example poc.yaml shapes",
		Long: `Show the cluster topologies this dpubnkctl build supports, with
HA notes and minimal poc.yaml snippets you can crib from.

The full schema (every field, what it does, who consumes it) lives in
internal/poc/schema.go in the source tree. This command is the field
quick-reference for customers who only have the binary.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTopologies(cmd.OutOrStdout())
		},
	}
}

func runTopologies(out io.Writer) error {
	if _, err := fmt.Fprint(out, topologiesTable); err != nil {
		return err
	}
	_, err := fmt.Fprint(out, topologiesExamples)
	return err
}

// topologiesShortSummary is the one-block summary suitable for appending
// to `dpubnkctl version`. Kept compact — full table available via
// `dpubnkctl topologies`.
const topologiesShortSummary = `Topologies supported:
  - Single-node lab       (1 host, role: both)
  - Minimum multi-node    (2 hosts: 1 CP + 1 worker)
  - Two control planes    (2 hosts, role: both — lake1 shape, not HA-safe)
  - Production HA         (3+ hosts, 3 control planes)
Run "dpubnkctl topologies" for the full table with example poc.yaml.`
