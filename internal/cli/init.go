package cli

import (
	"crypto/rand"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mwiget/dpubnkctl/internal/embedded"
	"github.com/mwiget/dpubnkctl/internal/poc"
)

func newInitCmd() *cobra.Command {
	var (
		dir      string
		customer string
		noGit    bool
	)
	cmd := &cobra.Command{
		Use:   "init <poc-name>",
		Short: "Create a new PoC repo (declarative state + journal + agent personas)",
		Long: `Create a new PoC repo at ./<poc-name> (or --dir <path>).

The repo contains:
  poc.yaml       declarative state (source of truth for teardown/redeploy)
  AGENTS.md      instructions for any agentic CLI driving this PoC
  CLAUDE.md      one-line @AGENTS.md include for Claude Code
  personas/      pre-sales-se, lab-tech, doc-specialist role definitions
  journal/       append-only markdown log written during the PoC
  inventory/     populated by 'dpubnkctl discover'
  artifacts/     bf.conf renders, kubespray inventory, kubeconfig, ...
  keys/          gitignored — drop FAR tgz, JWT, SSH keys here
  decisions.md   running decision log (owned by pre-sales SE persona)
  .gitignore     excludes all secret material from git

Initializes a git repo unless --no-git.`,
		Args: initArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if !validName(name) {
				return fmt.Errorf("invalid PoC name %q: use [a-z0-9-]+", name)
			}
			target := dir
			if target == "" {
				target = "./" + name
			}
			// Honor `~` / `~/...` even when the shell didn't expand it
			// (quoted args, non-bash shells, some IDE terminals). Anything
			// past that is filepath.Abs's job.
			target = expandTilde(target)
			abs, err := filepath.Abs(target)
			if err != nil {
				return err
			}
			if _, err := os.Stat(abs); err == nil {
				return fmt.Errorf("refusing to overwrite: %s already exists", abs)
			}

			if err := os.MkdirAll(abs, 0o755); err != nil {
				return err
			}

			// Create skeleton dirs.
			for _, d := range []string{"journal", "inventory", "artifacts", "keys", "personas"} {
				if err := os.MkdirAll(filepath.Join(abs, d), 0o755); err != nil {
					return err
				}
			}

			// Drop a placeholder so empty dirs survive git add.
			for _, d := range []string{"journal", "inventory", "artifacts"} {
				keep := filepath.Join(abs, d, ".gitkeep")
				if err := os.WriteFile(keep, nil, 0o644); err != nil {
					return err
				}
			}

			// Copy embedded files (AGENTS.md, CLAUDE.md, personas/, .gitignore).
			if err := copyEmbedded(abs); err != nil {
				return fmt.Errorf("copy templates: %w", err)
			}

			// Generate a random DPU OS password so the operator can
			// (a) console into a DPU when it's stuck mid-boot, and
			// (b) avoid silently inheriting whatever ad-hoc password
			// was hand-crafted with `openssl passwd -1`. Cleartext lands
			// at keys/dpu_password.txt (mode 0600, gitignored); the
			// MD5-crypt hash lands at keys/dpu_password.hash for bf.conf.
			pw, err := generateDPUPassword(abs)
			if err != nil {
				return fmt.Errorf("generate DPU password: %w", err)
			}

			// Write poc.yaml with binary defaults.
			p := poc.New(name)
			p.Metadata.Customer = customer
			if err := savePoC(abs, p, cmd.OutOrStdout()); err != nil {
				return err
			}

			// Seed decisions.md with empty template.
			decisions := fmt.Sprintf(`# Decisions — %s

Running log of scope, topology, and tradeoff decisions made during this PoC.
Owned by the pre-sales SE persona.

| Date | Decision | Rationale | Alternative rejected |
|------|----------|-----------|----------------------|
| %s | PoC created with dpubnkctl defaults (BNK 2.3.0, DOCA 3.2.0, release-manifest 2.3.0-3.2598.3-0.0.170) | binary-pinned baseline | manual stack composition |
`, name, time.Now().UTC().Format("2006-01-02"))
			if err := os.WriteFile(filepath.Join(abs, "decisions.md"), []byte(decisions), 0o644); err != nil {
				return err
			}

			// First journal entry.
			j := fmt.Sprintf(`# %s — PoC initialized

Created with dpubnkctl. Next step: pre-sales SE confirms scope and topology
in poc.yaml, then lab-tech runs `+"`dpubnkctl discover host <ip>`"+` for each
host the customer provides.
`, time.Now().UTC().Format("2006-01-02"))
			if err := os.WriteFile(filepath.Join(abs, "journal", time.Now().UTC().Format("2006-01-02")+"-init.md"), []byte(j), 0o644); err != nil {
				return err
			}

			if !noGit {
				if err := gitInit(abs); err != nil {
					return fmt.Errorf("git init: %w", err)
				}
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "PoC repo created at %s\n", abs)
			fmt.Fprintln(out)
			fmt.Fprintln(out, "DPU OS credentials (used by `provision dpu` to flash bf.conf,")
			fmt.Fprintln(out, "and what you'll type at the DPU serial console for diagnostics):")
			fmt.Fprintf(out, "  username: ubuntu\n")
			fmt.Fprintf(out, "  password: %s\n", pw)
			fmt.Fprintln(out, "  cleartext at keys/dpu_password.txt (mode 0600, gitignored)")
			fmt.Fprintln(out, "  MD5-crypt hash at keys/dpu_password.hash (referenced by poc.yaml)")
			fmt.Fprintln(out)
			fmt.Fprintln(out, "Next:")
			fmt.Fprintf(out, "  cd %s\n", target)
			fmt.Fprintln(out, "  $EDITOR poc.yaml          # set topology, expected hosts, network plan")
			fmt.Fprintln(out, "  dpubnkctl discover host <ip> --ssh-user <u> --ssh-key keys/<file>")
			fmt.Fprintln(out, "Or drive it with an agent:")
			fmt.Fprintln(out, "  dpubnkctl agent claude    # prints invocation for Claude Code")
			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "dir", "", "target directory (default ./<poc-name>)")
	cmd.Flags().StringVar(&customer, "customer", "", "customer name to record in poc.yaml metadata")
	cmd.Flags().BoolVar(&noGit, "no-git", false, "skip git init")
	return cmd
}

// initArgs surfaces a friendlier error than cobra's default
// "accepts 1 arg(s), received 0" when the operator forgets to name the
// PoC. The first ask after install is `dpubnkctl init` and a cryptic
// arity error is a bad first impression.
func initArgs(cmd *cobra.Command, args []string) error {
	switch len(args) {
	case 0:
		return fmt.Errorf(`PoC name required.

Usage:
  dpubnkctl init <poc-name> [--dir <path>] [--customer "<name>"]

By default the PoC repo is created at ./<poc-name> relative to your
current working directory. Pass --dir to put it elsewhere.

Examples:
  dpubnkctl init customer-acme                           # → ./customer-acme
  dpubnkctl init customer-acme --customer "Acme Corp"    # → ./customer-acme
  dpubnkctl init lab1 --dir ~/pocs/lab1                  # → ~/pocs/lab1
  dpubnkctl init lab1 --dir /srv/pocs/lab1               # absolute path

The name must match [a-z0-9-]+ and lands in poc.yaml.metadata.name.
The target directory must not already exist (init refuses to overwrite).
Run "dpubnkctl init --help" for all flags`)
	case 1:
		return nil
	default:
		return fmt.Errorf("only one <poc-name> is accepted; got %d arguments", len(args))
	}
}

// expandTilde resolves a leading `~` or `~/...` to the operator's home
// directory. Anything else is returned unchanged. Mirrors what bash
// would do unquoted; covers the case where the path arrived quoted or
// from a non-bash shell.
func expandTilde(p string) string {
	if p == "~" {
		if h, err := os.UserHomeDir(); err == nil {
			return h
		}
		return p
	}
	if strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[2:])
		}
	}
	return p
}

func validName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-':
		default:
			return false
		}
	}
	return true
}

// copyEmbedded walks the embedded FS and writes everything under "files/"
// into target, with one rename: "files/poc.gitignore" → ".gitignore".
func copyEmbedded(target string) error {
	return fs.WalkDir(embedded.FS, "files", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(path, "files/")
		if rel == "" || rel == "files" {
			return nil
		}
		if rel == "poc.gitignore" {
			rel = ".gitignore"
		}
		dst := filepath.Join(target, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		data, err := embedded.FS.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		return os.WriteFile(dst, data, 0o644)
	})
}

func gitInit(dir string) error {
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"add", "."},
		{"commit", "-q", "-m", "dpubnkctl init"},
	} {
		c := exec.Command("git", args...)
		c.Dir = dir
		c.Stdout = os.Stderr
		c.Stderr = os.Stderr
		if err := c.Run(); err != nil {
			return fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
		}
	}
	return nil
}

// dpuPasswordCharset is the 54-char alphanumeric set we draw the DPU
// password from. Omits visually-ambiguous characters (0/O/o, 1/l/I) so
// the operator can confidently type the password at a DPU serial console
// — which is the whole reason we generate the password (vs. operator-
// supplied) in the first place.
const dpuPasswordCharset = "abcdefghjkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// dpuSaltCharset is the 64-char set MD5-crypt salts pull from per the
// Poul-Henning Kamp convention (./0-9A-Za-z).
const dpuSaltCharset = "./0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// generateDPUPassword creates a random 12-char password, MD5-crypts it
// (matching what `openssl passwd -1` produces — the format bf.conf's
// `ubuntu_PASSWORD=` expects), and writes:
//
//   - keys/dpu_password.hash  (mode 0644): `$1$<salt>$<hash>` — referenced
//     by poc.yaml.provisioning.dpu_password_hash_ref. This is the value
//     baked into bf.conf and flashed onto the DPU's /etc/shadow.
//
//   - keys/dpu_password.txt   (mode 0600): cleartext, gitignored. The
//     operator types this at the DPU serial console for diagnostics or
//     manual recovery.
//
// Returns the cleartext so the init command can echo it to stdout for
// the operator to record.
//
// Shells to `openssl passwd -1 -salt <salt>` for the hash because Go's
// stdlib doesn't ship MD5-crypt and openssl is universally available on
// Linux + macOS. Falls back with a clear error message if openssl isn't
// on PATH.
func generateDPUPassword(repoDir string) (string, error) {
	if _, err := exec.LookPath("openssl"); err != nil {
		return "", fmt.Errorf("openssl not on PATH — needed to compute MD5-crypt of the DPU password. " +
			"Install openssl, or hand-craft keys/dpu_password.hash with `openssl passwd -1 '<pw>'` " +
			"and keys/dpu_password.txt with the cleartext")
	}

	cleartext, err := randomString(12, dpuPasswordCharset)
	if err != nil {
		return "", fmt.Errorf("crypto/rand for password: %w", err)
	}
	salt, err := randomString(8, dpuSaltCharset)
	if err != nil {
		return "", fmt.Errorf("crypto/rand for salt: %w", err)
	}

	// `openssl passwd -1 -salt <salt> <password>` prints "$1$<salt>$<hash>".
	// We pipe the password through stdin via -stdin so it never appears on
	// the host's argv list (visible in /proc/<pid>/cmdline + ps + auditd).
	cmd := exec.Command("openssl", "passwd", "-1", "-salt", salt, "-stdin")
	cmd.Stdin = strings.NewReader(cleartext + "\n")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("openssl passwd: %w", err)
	}
	hash := strings.TrimSpace(string(out))
	if !strings.HasPrefix(hash, "$1$") {
		return "", fmt.Errorf("unexpected openssl passwd output: %q", hash)
	}

	// keys/ holds offline-crackable material: SSH private keys, the MD5-
	// crypt DPU password hash, the FAR tarball, the TEEM JWT. 0o700 so
	// other local users on the jumphost can't even list the contents;
	// each file is 0o600 too as belt-and-suspenders.
	keysDir := filepath.Join(repoDir, "keys")
	if err := os.MkdirAll(keysDir, 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(keysDir, "dpu_password.hash"), []byte(hash+"\n"), 0o600); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(keysDir, "dpu_password.txt"), []byte(cleartext+"\n"), 0o600); err != nil {
		return "", err
	}
	return cleartext, nil
}

// randomString returns n characters drawn uniformly at random from
// charset, using crypto/rand. Uses big.Int.Rand to avoid the modulo bias
// `rand.Read(b) % len(charset)` introduces (small but real, especially
// when the password lands at a serial-console authentication boundary
// where every bit of entropy matters).
func randomString(n int, charset string) (string, error) {
	if n <= 0 {
		return "", fmt.Errorf("randomString: n must be > 0")
	}
	max := big.NewInt(int64(len(charset)))
	out := make([]byte, n)
	for i := 0; i < n; i++ {
		idx, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		out[i] = charset[idx.Int64()]
	}
	return string(out), nil
}
