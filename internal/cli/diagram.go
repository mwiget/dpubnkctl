package cli

import (
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mwiget/dpubnkctl/internal/poc"
)

func newDiagramCmd() *cobra.Command {
	var pocDir string
	cmd := &cobra.Command{
		Use:   "diagram",
		Short: "Print an ASCII topology diagram for this PoC (cluster + data-plane VLANs)",
		Long: `Render the current PoC's topology as plain ASCII:

  - K8s cluster: hosts side-by-side with their DPU children, kubeadm
    join arrows, apiserver callout.
  - Data-plane VLANs: per-VLAN tables of every host/DPU/self-IP.

Output renders in any terminal, plain text editor, log viewer, or
inside a fenced code block in markdown — no markdown renderer needed.
Useful for sharing topology during scope review, pasting into PoC
reports, or eyeballing a poc.yaml without spinning anything up.

Works on any populated poc.yaml — not tied to dpubnkctl e2e.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDiagram(cmd.OutOrStdout(), pocDir)
		},
	}
	cmd.Flags().StringVar(&pocDir, "poc", "", "PoC repo path (default: current directory)")
	return cmd
}

func runDiagram(out io.Writer, pocDir string) error {
	repo, err := resolvePoCDir(pocDir)
	if err != nil {
		return err
	}
	p, err := poc.Load(repo)
	if err != nil {
		return fmt.Errorf("not a PoC repo (%s): %w", repo, err)
	}
	io.WriteString(out, RenderFullDiagram(p))
	// Also refresh the on-disk diagram.txt at the PoC root so the file
	// stays current whenever the operator runs `dpubnkctl diagram` for
	// any reason. Best-effort: a failure to write doesn't undo the
	// stdout output the operator just saw.
	if err := writeDiagramFile(repo, p); err != nil {
		fmt.Fprintf(out, "\nWARN: refresh %s failed: %v\n", diagramFileName, err)
	}
	return nil
}

// RenderFullDiagram concatenates the three diagram sections that
// `diagram.txt` ships with: the K8s cluster ASCII boxes, the
// mgmt-plane table (out-of-band IPs operators use to SSH), and the
// data-plane VLAN tables. Single string so callers can write it to
// stdout or to a file uniformly.
func RenderFullDiagram(p *poc.PoC) string {
	var b strings.Builder
	b.WriteString(RenderClusterASCII(p))
	b.WriteString("\n")
	b.WriteString(RenderMgmtPlaneASCII(p))
	b.WriteString("\n")
	b.WriteString(RenderVLANsASCII(p))
	return b.String()
}

// diagramFileName is the basename of the auto-generated diagram inside
// every PoC repo. Lives at the repo root (alongside poc.yaml), not in
// artifacts/, since it's the human-facing summary — operators check it
// to confirm what's in poc.yaml without parsing yaml by hand.
const diagramFileName = "diagram.txt"

// writeDiagramFile renders RenderFullDiagram(p) into <repo>/diagram.txt.
// Best-effort: callers usually invoke this right after p.Save(repo);
// a failure here shouldn't undo the save, so they should `_ =` the
// return value.
func writeDiagramFile(repo string, p *poc.PoC) error {
	path := filepath.Join(repo, diagramFileName)
	return os.WriteFile(path, []byte(RenderFullDiagram(p)), 0o644)
}

// savePoC persists poc.yaml AND regenerates diagram.txt so the human
// readable view stays in lockstep with the source of truth. All cli
// subcommands route through this instead of calling p.Save() directly
// so a future phase that mutates poc.yaml automatically refreshes the
// diagram (no new wiring required).
//
// The diagram write is best-effort — if it fails (disk full, permission)
// we log a warning but return success, since the canonical state is
// poc.yaml. errOut is optional; nil to swallow the warning.
func savePoC(repo string, p *poc.PoC, errOut io.Writer) error {
	if err := p.Save(repo); err != nil {
		return err
	}
	if err := writeDiagramFile(repo, p); err != nil && errOut != nil {
		fmt.Fprintf(errOut, "WARN: refresh %s failed: %v\n", diagramFileName, err)
	}
	return nil
}

// RenderClusterASCII emits the k8s cluster topology — hosts and their
// DPU children laid out side-by-side, with `kubeadm join` arrows between
// each host and its DPUs and a one-line apiserver callout at the bottom.
// One column per host; the columns are joined with a small horizontal
// gap. Suitable for ≤6 hosts; beyond that the lines get wide.
func RenderClusterASCII(p *poc.PoC) string {
	var b strings.Builder
	title := fmt.Sprintf("K8s cluster: %s", p.Metadata.Name)
	fmt.Fprintln(&b, title)
	fmt.Fprintln(&b, strings.Repeat("=", len(title)))
	fmt.Fprintln(&b)

	if len(p.Hosts) == 0 {
		fmt.Fprintln(&b, "  (no hosts in poc.yaml — run `dpubnkctl discover wizard` first)")
		return b.String()
	}

	columns := make([][]string, 0, len(p.Hosts))
	for i := range p.Hosts {
		columns = append(columns, renderHostColumn(&p.Hosts[i]))
	}
	for _, line := range joinColumns(columns, "    ") {
		fmt.Fprintln(&b, "  "+line)
	}
	fmt.Fprintln(&b)
	if addr := p.Network.ClusterAPIServerAddress; addr != "" {
		count := 0
		for _, h := range p.Hosts {
			count++ // host node
			count += len(h.DPUs)
		}
		fmt.Fprintf(&b, "  apiserver: %s:6443  (all %d node(s) connect here)\n", addr, count)
	}
	return b.String()
}

// renderHostColumn returns one host's vertical column: host box on top,
// then for each DPU a centered arrow + DPU box. All boxes in the column
// are rendered at the SAME outer width so the arrows align perfectly
// under each box's center.
func renderHostColumn(h *poc.Host) []string {
	// Host mgmt line: prefix with the kernel iface name (e.g. "eth0")
	// when discover captured it, so the host block reads symmetrically
	// with the DPU block ("oob_net0 <ip>"). Older PoCs without
	// MgmtIface populated fall back to the bare IP.
	mgmtLine := h.SSH.Address
	if iface := strings.TrimSpace(h.MgmtIface); iface != "" {
		mgmtLine = iface + " " + h.SSH.Address
	}
	hostBody := []string{h.Role, mgmtLine}

	type dpuSpec struct {
		title string
		body  []string
	}
	dpus := make([]dpuSpec, 0, len(h.DPUs))
	for _, d := range h.DPUs {
		label := d.Hostname
		if label == "" {
			label = h.Name + "-bf3"
		}
		lag := "non-LAG"
		if d.LAG {
			lag = "LAG"
		}
		body := []string{"DPU worker", "(" + lag + ")"}
		// Prefer the DPU's externally-reachable oob_net0 address; label
		// with the literal iface name so the block reads "oob_net0 <ip>"
		// (parallel to the host block's "<iface> <ip>"). When oob_ip
		// isn't captured yet, fall back to the tmfifo address with the
		// "tmfifo_net0" iface label so the operator can still find it.
		if oob := strings.TrimSpace(d.OOBIP); oob != "" {
			body = append(body, "oob_net0 "+stripCIDR(oob))
		} else if tm := stripCIDR(d.TmfifoIP); tm != "" {
			body = append(body, "tmfifo_net0 "+tm)
		}
		dpus = append(dpus, dpuSpec{title: label, body: body})
	}

	// Compute a common inner width across the host box + every DPU box
	// so they all share the same outer width and the arrows line up.
	inner := boxInnerWidth(h.Name, hostBody)
	for _, d := range dpus {
		if w := boxInnerWidth(d.title, d.body); w > inner {
			inner = w
		}
	}

	out := renderBoxAt(h.Name, hostBody, inner)
	for _, d := range dpus {
		// All three arrow lines start their `|` (or `v`) at the box's
		// center column so they stack vertically. centerLine would
		// center the whole string and misalign the `|` in "| kubeadm
		// join" against the lone `|`/`v` above and below.
		out = append(out,
			arrowAtCenter(inner+2, "|"),
			arrowAtCenter(inner+2, "| kubeadm join"),
			arrowAtCenter(inner+2, "v"),
		)
		out = append(out, renderBoxAt(d.title, d.body, inner)...)
	}
	return out
}

// arrowAtCenter returns a string whose first char lands at the box
// center column, with `content` trailing to the right. Used for the
// `|`, `| kubeadm join`, `v` lines between host and DPU boxes so they
// stack vertically regardless of any trailing label.
func arrowAtCenter(colWidth int, content string) string {
	centerCol := (colWidth - 1) / 2
	return strings.Repeat(" ", centerCol) + content
}

// boxInnerWidth returns the inner width (between the two `|` borders)
// that renderBoxAt would need to fit title + every body line with the
// canonical padding.
func boxInnerWidth(title string, body []string) int {
	w := len(title)
	for _, l := range body {
		if n := len(l); n > w {
			w = n
		}
	}
	w += 4 // 2 chars padding either side of the longest line
	if w < 17 {
		w = 17
	}
	return w
}

// renderBoxAt draws an ASCII box with the given inner width. Title is
// the first inner line; each body string is one more inner line. All
// inner lines are centered.
func renderBoxAt(title string, body []string, inner int) []string {
	bar := "+" + strings.Repeat("-", inner) + "+"
	lines := []string{bar, "|" + center(title, inner) + "|"}
	for _, l := range body {
		lines = append(lines, "|"+center(l, inner)+"|")
	}
	lines = append(lines, bar)
	return lines
}

func center(s string, width int) string {
	pad := width - len(s)
	if pad <= 0 {
		return s
	}
	left := pad / 2
	return strings.Repeat(" ", left) + s + strings.Repeat(" ", pad-left)
}

// joinColumns merges N columns horizontally with sep between them. Each
// column's width is its widest line; shorter lines (and missing rows on
// shorter columns) are right-padded with spaces so everything aligns.
func joinColumns(cols [][]string, sep string) []string {
	height := 0
	widths := make([]int, len(cols))
	for i, c := range cols {
		if len(c) > height {
			height = len(c)
		}
		for _, line := range c {
			if w := lineWidth(line); w > widths[i] {
				widths[i] = w
			}
		}
	}
	out := make([]string, height)
	for row := 0; row < height; row++ {
		var parts []string
		for i, c := range cols {
			var line string
			if row < len(c) {
				line = c[row]
			}
			if pad := widths[i] - lineWidth(line); pad > 0 {
				line += strings.Repeat(" ", pad)
			}
			parts = append(parts, line)
		}
		out[row] = strings.Join(parts, sep)
	}
	return out
}

// lineWidth returns the visible width of a line. We use len() because
// our renderer never emits non-ASCII; if that changes, swap to
// utf8.RuneCountInString.
func lineWidth(s string) int { return len(s) }

// RenderMgmtPlaneASCII emits the out-of-band management plane table:
// each host's SSH address (which is also its mgmt IP) and each DPU's
// oob_net0 IP captured at cluster-join time. This is the "how do I get
// a shell on it" view, separate from the data-plane VLAN tables which
// are about east-west traffic IPs.
//
// DPUs with no oob_ip yet (not joined, or pre-discovery) show "—"
// rather than being silently omitted, so the table doubles as a
// progress indicator across phases.
func RenderMgmtPlaneASCII(p *poc.PoC) string {
	var b strings.Builder
	title := "Mgmt-plane (out-of-band SSH addresses)"
	fmt.Fprintln(&b, title)
	fmt.Fprintln(&b, strings.Repeat("=", len(title)))
	fmt.Fprintln(&b)

	if len(p.Hosts) == 0 {
		fmt.Fprintln(&b, "  (no hosts in poc.yaml)")
		return b.String()
	}

	type row struct{ name, role, iface, addr string }
	var rows []row
	nameW, roleW, ifaceW, addrW := len("Node"), len("Role"), len("Iface"), len("Address")
	for _, h := range p.Hosts {
		iface := strings.TrimSpace(h.MgmtIface)
		if iface == "" {
			iface = "—"
		}
		rows = append(rows, row{name: h.Name, role: h.Role, iface: iface, addr: h.SSH.Address})
		for _, d := range h.DPUs {
			dname := d.Hostname
			if dname == "" {
				dname = h.Name + "-bf3"
			}
			addr := stripCIDR(strings.TrimSpace(d.OOBIP))
			if addr == "" {
				addr = "—"
			}
			// DPU mgmt is always oob_net0 on BlueField — constant, not
			// learned. Hard-code the label so the table is complete.
			rows = append(rows, row{name: dname, role: "dpu", iface: "oob_net0", addr: addr})
		}
	}
	for _, r := range rows {
		if n := len(r.name); n > nameW {
			nameW = n
		}
		if n := len(r.role); n > roleW {
			roleW = n
		}
		if n := len(r.iface); n > ifaceW {
			ifaceW = n
		}
		if n := len(r.addr); n > addrW {
			addrW = n
		}
	}
	fmt.Fprintf(&b, "  %-*s    %-*s    %-*s    %-*s\n",
		nameW, "Node", roleW, "Role", ifaceW, "Iface", addrW, "Address")
	fmt.Fprintf(&b, "  %s    %s    %s    %s\n",
		strings.Repeat("-", nameW), strings.Repeat("-", roleW),
		strings.Repeat("-", ifaceW), strings.Repeat("-", addrW))
	for _, r := range rows {
		fmt.Fprintf(&b, "  %-*s    %-*s    %-*s    %-*s\n",
			nameW, r.name, roleW, r.role, ifaceW, r.iface, addrW, r.addr)
	}
	return b.String()
}

// RenderVLANsASCII emits aligned tables for every VLAN, listing every
// host / DPU / TMM self-IP that lives in it with the host part of its
// IP (last octet). One table per VLAN; tables sorted by tag.
func RenderVLANsASCII(p *poc.PoC) string {
	var b strings.Builder
	title := "Data-plane VLANs"
	fmt.Fprintln(&b, title)
	fmt.Fprintln(&b, strings.Repeat("=", len(title)))
	fmt.Fprintln(&b)

	type member struct{ label, ip string }
	byTag := map[int][]member{}
	subnetByTag := map[int]string{}
	roleByTag := map[int]string{}
	for _, v := range p.Network.VLANs {
		subnetByTag[v.ID] = v.Subnet
		roleByTag[v.ID] = v.Name
	}
	for _, h := range p.Hosts {
		if h.DataPlane != nil {
			for _, v := range h.DataPlane.VLANs {
				byTag[v.Tag] = append(byTag[v.Tag], member{label: h.Name, ip: lastOctetOf(v.IP)})
			}
		}
		for _, d := range h.DPUs {
			label := d.Hostname
			if label == "" {
				label = h.Name + "-bf3"
			}
			for _, v := range d.VLANs {
				byTag[v.Tag] = append(byTag[v.Tag], member{label: label, ip: lastOctetOf(v.IP)})
			}
		}
	}
	add := func(role, ip string) {
		if ip == "" {
			return
		}
		for _, v := range p.Network.VLANs {
			if v.Name == role {
				byTag[v.ID] = append(byTag[v.ID], member{label: "TMM self-IP", ip: lastOctetOf(ip)})
			}
		}
	}
	add("external", p.BNK.ExternalSelfIP)
	add("internal", p.BNK.InternalSelfIP)

	if len(byTag) == 0 {
		fmt.Fprintln(&b, "  (no VLANs in poc.yaml)")
		return b.String()
	}

	var tags []int
	for t := range byTag {
		tags = append(tags, t)
	}
	sort.Ints(tags)

	for _, t := range tags {
		header := fmt.Sprintf("%s VLAN %d — %s", roleByTag[t], t, subnetByTag[t])
		fmt.Fprintln(&b, "  "+header)
		fmt.Fprintln(&b, "  "+strings.Repeat("-", len(header)))
		labelW := 0
		for _, m := range byTag[t] {
			if n := len(m.label); n > labelW {
				labelW = n
			}
		}
		for _, m := range byTag[t] {
			fmt.Fprintf(&b, "    %-*s    .%s\n", labelW, m.label, m.ip)
		}
		fmt.Fprintln(&b)
	}
	return b.String()
}

// lastOctetOf pulls the last IPv4 octet out of a CIDR ("10.10.40.66/24")
// or bare IP ("10.10.40.66") string. Returns "?" if it can't parse.
func lastOctetOf(s string) string {
	if i := strings.IndexByte(s, '/'); i > 0 {
		s = s[:i]
	}
	ip := net.ParseIP(s)
	if ip == nil {
		return "?"
	}
	if v4 := ip.To4(); v4 != nil {
		return fmt.Sprintf("%d", v4[3])
	}
	return s
}
