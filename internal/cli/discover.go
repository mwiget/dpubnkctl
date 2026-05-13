package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mwiget/dpubnkctl/internal/discover"
	"github.com/mwiget/dpubnkctl/internal/poc"
	"github.com/mwiget/dpubnkctl/internal/ssh"
)

func newDiscoverCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "discover",
		Short: "Probe hosts and DPUs over SSH, build inventory",
	}
	cmd.AddCommand(newDiscoverHostCmd())
	cmd.AddCommand(newDiscoverRangeCmd())
	cmd.AddCommand(newDiscoverWizardCmd())
	return cmd
}

type discoverHostFlags struct {
	pocDir     string
	sshUser    string
	sshKey     string
	sshPort    int
	jumphost   string
	jumpUser   string
	jumpKey    string
	bmcAddr    string // manual override
	importKey  bool
	noUpdate   bool
	role       string
	timeout    time.Duration
}

func newDiscoverHostCmd() *cobra.Command {
	f := &discoverHostFlags{}
	cmd := &cobra.Command{
		Use:   "host <address>",
		Short: "Probe a single host over SSH (auto-discover BMC, classify DPUs)",
		Long: `Connect to <address> over SSH and run a battery of read-only probes:
host kernel/OS/model, network interfaces, tool inventory (mlxconfig, bfb-install,
ipmitool, rshim), BMC IP via 'ipmitool lan print', BlueField DPUs via lspci,
per-DPU NV-config via mlxconfig, and rshim state.

Result is written to inventory/<hostname>/discover.json and merged into
poc.yaml. A journal entry is appended.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDiscoverHost(cmd.Context(), cmd.OutOrStdout(), args[0], f)
		},
	}
	cmd.Flags().StringVar(&f.pocDir, "poc", "", "PoC repo path (default: current directory)")
	cmd.Flags().StringVar(&f.sshUser, "ssh-user", "root", "SSH user")
	cmd.Flags().StringVar(&f.sshKey, "ssh-key", "", "Path to SSH private key (required)")
	cmd.Flags().IntVar(&f.sshPort, "ssh-port", 22, "SSH port")
	cmd.Flags().StringVar(&f.jumphost, "jumphost", "", "Jumphost address (optional, format host[:port])")
	cmd.Flags().StringVar(&f.jumpUser, "jumphost-user", "", "Jumphost SSH user (defaults to --ssh-user)")
	cmd.Flags().StringVar(&f.jumpKey, "jumphost-key", "", "Jumphost SSH private key (defaults to --ssh-key)")
	cmd.Flags().StringVar(&f.bmcAddr, "bmc-address", "", "Override BMC address (skips ipmitool auto-discovery)")
	cmd.Flags().BoolVar(&f.importKey, "import-key", false, "Copy --ssh-key into the PoC's keys/ dir for redeploy reproducibility")
	cmd.Flags().BoolVar(&f.noUpdate, "no-update", false, "Print findings only; do not modify poc.yaml or inventory/")
	cmd.Flags().StringVar(&f.role, "role", "", "Cluster role to record: control-plane | worker | both (left empty for SE to decide)")
	cmd.Flags().DurationVar(&f.timeout, "timeout", 60*time.Second, "Per-probe timeout cap")
	_ = cmd.MarkFlagRequired("ssh-key")
	return cmd
}

func runDiscoverHost(ctx context.Context, out io.Writer, addr string, f *discoverHostFlags) error {
	repo, err := resolvePoCDir(f.pocDir)
	if err != nil {
		return err
	}
	p, err := poc.Load(repo)
	if err != nil {
		return fmt.Errorf("not a PoC repo (%s): %w", repo, err)
	}

	// Build SSH config (jumphost chained if requested).
	known := filepath.Join(repo, "inventory", "known_hosts")
	cfg := ssh.Config{
		Address:    addr,
		Port:       f.sshPort,
		User:       f.sshUser,
		KeyPath:    f.sshKey,
		KnownHosts: known,
		Timeout:    f.timeout,
	}
	if f.jumphost != "" {
		jumpUser := f.jumpUser
		if jumpUser == "" {
			jumpUser = f.sshUser
		}
		jumpKey := f.jumpKey
		if jumpKey == "" {
			jumpKey = f.sshKey
		}
		cfg.Jumphost = &ssh.Config{
			Address:    f.jumphost,
			Port:       22,
			User:       jumpUser,
			KeyPath:    jumpKey,
			KnownHosts: known,
			Timeout:    f.timeout,
		}
	}

	fmt.Fprintf(out, "Connecting to %s@%s ... ", cfg.User, addr)
	dialCtx, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()
	client, err := ssh.Dial(dialCtx, cfg)
	if err != nil {
		fmt.Fprintln(out, "FAIL")
		return err
	}
	defer client.Close()
	fmt.Fprintln(out, "ok")

	fmt.Fprintln(out, "Probing host, BMC, and DPUs ...")
	probeCtx, probeCancel := context.WithTimeout(ctx, 5*time.Minute)
	defer probeCancel()
	result, err := discover.DiscoverHost(probeCtx, discover.HostOptions{Address: addr, Runner: client})
	if err != nil {
		return err
	}

	// Apply manual BMC override if provided.
	if f.bmcAddr != "" {
		result.BMC = &discover.BMCInfo{Source: "manual", IP: f.bmcAddr}
	}

	printSummary(out, result)

	if f.noUpdate {
		fmt.Fprintln(out, "\n--no-update: poc.yaml and inventory/ left unchanged.")
		return nil
	}

	// Persist inventory JSON.
	hostname := result.Host.Hostname
	if hostname == "" {
		hostname = sanitizeHostKey(addr)
	}
	invDir := filepath.Join(repo, "inventory", hostname)
	if err := os.MkdirAll(invDir, 0o755); err != nil {
		return err
	}
	jsonPath := filepath.Join(invDir, "discover.json")
	if err := writeJSON(jsonPath, result); err != nil {
		return err
	}
	fmt.Fprintf(out, "\nWrote %s\n", jsonPath)

	// Optionally import the SSH key into the PoC for reproducibility.
	keyRef := f.sshKey
	if f.importKey {
		dst := filepath.Join(repo, "keys", hostname)
		if err := copyFile(f.sshKey, dst, 0o600); err != nil {
			return fmt.Errorf("import-key: %w", err)
		}
		keyRef = filepath.Join("keys", hostname)
		fmt.Fprintf(out, "Imported SSH key to %s (gitignored)\n", dst)
	}

	// Merge into poc.yaml.
	updatePoCWithHost(p, hostname, addr, f, keyRef, result)
	if err := p.Save(repo); err != nil {
		return err
	}
	fmt.Fprintf(out, "Updated poc.yaml: host %q (%s)\n", hostname, result.Classification())

	// Append journal entry.
	if err := appendDiscoverJournal(repo, hostname, addr, result); err != nil {
		return err
	}

	return nil
}

func sanitizeHostKey(s string) string {
	r := strings.NewReplacer(":", "_", "/", "_", " ", "_")
	return r.Replace(s)
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, in, mode)
}

// updatePoCWithHost merges discovery output into p.Hosts. Returns true
// when the entry was merged into an existing host (matched by SSH
// address), false when a new host was appended.
//
// Merge policy is defensively non-destructive: anything the SE has
// already curated in poc.yaml — role, data_plane block + VLANs, per-DPU
// mode/lag/hostname/tmfifo_ip/vlans, BMC creds, jumphost — is
// preserved. Discovery fills only fields that are currently empty.
// DPUs are merged by PCI: discovered PCIs not in the existing host
// append as starter entries; PCIs already present are left untouched.
//
// This lets the operator re-run `dpubnkctl discover` (or the wizard)
// against an already-populated PoC without losing any hand-curated
// network plan. To force a clean rebuild, delete hosts[] from poc.yaml
// first.
func updatePoCWithHost(p *poc.PoC, name, addr string, f *discoverHostFlags, keyRef string, r *discover.Result) (merged bool) {
	p.Status.Discover = "in_progress" // becomes "completed" once SE confirms

	for i := range p.Hosts {
		if p.Hosts[i].SSH.Address != addr {
			continue
		}
		existing := &p.Hosts[i]
		if existing.Name == "" {
			existing.Name = name
		}
		if existing.Role == "" {
			existing.Role = f.role
		}
		if existing.SSH.User == "" {
			existing.SSH.User = f.sshUser
		}
		if existing.SSH.Port == 0 {
			existing.SSH.Port = f.sshPort
		}
		if existing.SSH.KeyRef == "" {
			existing.SSH.KeyRef = keyRef
		}
		if existing.SSH.Jumphost == "" {
			existing.SSH.Jumphost = f.jumphost
		}
		if existing.BMC == nil && r.BMC != nil {
			existing.BMC = &poc.BMC{Address: r.BMC.IP, Protocol: "redfish"}
		}
		// DPUs: merge by PCI. Existing entries are sacred (operator may
		// have set mode, lag, hostname, tmfifo_ip, vlans). Add discovered
		// PCIs that aren't in the existing list yet.
		for _, d := range r.DPUs {
			if hasExistingDPU(existing.DPUs, d.PCI) {
				continue
			}
			existing.DPUs = append(existing.DPUs, poc.DPU{
				Serial: pickSerial(d),
				PCI:    d.PCI,
				Mode:   pickMode(d),
				LAG:    pickLAG(d),
			})
		}
		return true
	}

	// No match — build a fresh host entry from discovery.
	host := poc.Host{
		Name: name,
		Role: f.role,
		SSH: poc.SSH{
			Address:  addr,
			Port:     f.sshPort,
			User:     f.sshUser,
			KeyRef:   keyRef,
			Jumphost: f.jumphost,
		},
	}
	if r.BMC != nil {
		host.BMC = &poc.BMC{Address: r.BMC.IP, Protocol: "redfish"}
	}
	for _, d := range r.DPUs {
		host.DPUs = append(host.DPUs, poc.DPU{
			Serial: pickSerial(d),
			PCI:    d.PCI,
			Mode:   pickMode(d),
			LAG:    pickLAG(d),
		})
	}
	p.Hosts = append(p.Hosts, host)
	return false
}

func hasExistingDPU(dpus []poc.DPU, pci string) bool {
	for _, d := range dpus {
		if d.PCI == pci {
			return true
		}
	}
	return false
}

// pickSerial pulls the DPU serial from rshim misc, falling back to "".
func pickSerial(d discover.DPUDetail) string {
	if d.RshimMisc == nil {
		return ""
	}
	for _, k := range []string{"UUID", "DEV_NAME"} {
		if v, ok := d.RshimMisc[k]; ok {
			return v
		}
	}
	return ""
}

func pickMode(d discover.DPUDetail) string {
	if d.Mlxconfig == nil {
		return ""
	}
	switch d.Mlxconfig.InternalCPUModel {
	case "EMBEDDED_CPU":
		return "dpu"
	case "SEPARATED_HOST":
		return "nic"
	}
	return ""
}

func pickLAG(d discover.DPUDetail) bool {
	return d.Mlxconfig != nil && d.Mlxconfig.LAGResourceAllocation == "ENABLE"
}

func printSummary(out io.Writer, r *discover.Result) {
	fmt.Fprintf(out, "\n%s — %s\n", r.Address, r.Classification())
	fmt.Fprintf(out, "  hostname: %s\n", orDash(r.Host.Hostname))
	fmt.Fprintf(out, "  os:       %s (kernel %s)\n", orDash(r.Host.OS.PrettyName), orDash(r.Host.Kernel))
	if r.Host.Model != "" {
		fmt.Fprintf(out, "  model:    %s\n", r.Host.Model)
	}
	fmt.Fprintf(out, "  tools:    mlxconfig=%s bfb-install=%s ipmitool=%s rshim=%s\n",
		yesNo(r.Host.Tools.Mlxconfig), yesNo(r.Host.Tools.BFBInstall),
		yesNo(r.Host.Tools.Ipmitool), yesNo(r.Host.Tools.Rshim))
	fmt.Fprintf(out, "  rshim:    loaded=%v devices=%v\n", r.Host.Rshim.Loaded, r.Host.Rshim.Devices)
	if r.BMC != nil {
		fmt.Fprintf(out, "  bmc:      %s (via %s)\n", r.BMC.IP, r.BMC.Source)
	} else {
		fmt.Fprintln(out, "  bmc:      not discovered")
	}
	for _, d := range r.DPUs {
		fmt.Fprintf(out, "  dpu %s [%s] %s\n", d.PCI, d.DeviceID, d.Description)
		if d.Mlxconfig != nil {
			fmt.Fprintf(out, "       cpu_model=%s link=%s/%s lag=%s vfs=%d sf=%d\n",
				orDash(d.Mlxconfig.InternalCPUModel),
				orDash(d.Mlxconfig.LinkTypeP1), orDash(d.Mlxconfig.LinkTypeP2),
				orDash(d.Mlxconfig.LAGResourceAllocation),
				d.Mlxconfig.NumOfVFs, d.Mlxconfig.PFTotalSF)
			if len(d.Mlxconfig.PendingReboot) > 0 {
				fmt.Fprintf(out, "       pending reboot: %s\n", strings.Join(d.Mlxconfig.PendingReboot, ", "))
			}
		}
	}
	for _, w := range r.Warnings {
		fmt.Fprintf(out, "  ! %s\n", w)
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func yesNo(s string) string {
	if s == "" {
		return "no"
	}
	return "yes"
}

func appendDiscoverJournal(repo, hostname, addr string, r *discover.Result) error {
	date := time.Now().UTC().Format("2006-01-02")
	path := filepath.Join(repo, "journal", date+"-discover.md")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	fmt.Fprintf(f, "## lab-tech: discover host %s (%s)\n", hostname, addr)
	fmt.Fprintf(f, "- Time: %s\n", r.DiscoveredAt.Format(time.RFC3339))
	fmt.Fprintf(f, "- Result: %s\n", r.Classification())
	fmt.Fprintf(f, "- OS: %s, kernel %s\n", orDash(r.Host.OS.PrettyName), orDash(r.Host.Kernel))
	if r.BMC != nil {
		fmt.Fprintf(f, "- BMC: %s (via %s)\n", r.BMC.IP, r.BMC.Source)
	}
	fmt.Fprintf(f, "- DPUs: %d\n", len(r.DPUs))
	if len(r.Warnings) > 0 {
		fmt.Fprintf(f, "- Warnings:\n")
		for _, w := range r.Warnings {
			fmt.Fprintf(f, "  - %s\n", w)
		}
	}
	fmt.Fprintf(f, "- Artifact: inventory/%s/discover.json\n", hostname)
	fmt.Fprintln(f, "- Next: pre-sales SE confirms classification + sets role in poc.yaml")
	fmt.Fprintln(f)
	return nil
}
