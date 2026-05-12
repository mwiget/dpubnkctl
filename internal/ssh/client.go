// Package ssh wraps golang.org/x/crypto/ssh with the small surface
// dpubnkctl needs: key-only auth, optional jumphost, Run() for one-shot
// command execution, and a known_hosts TOFU file under the PoC repo.
package ssh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	xssh "golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

type Config struct {
	Address      string // host:port or host
	Port         int
	User         string
	KeyPath      string // path to private key (PEM/OpenSSH)
	Jumphost     *Config
	KnownHosts   string // path to known_hosts file (created if missing, TOFU)
	Timeout      time.Duration
	StrictHostKey bool // false for first-touch convenience; true for production
}

type Client struct {
	cfg  Config
	conn *xssh.Client
}

// Dial opens the SSH session, chaining through Jumphost if set.
func Dial(ctx context.Context, cfg Config) (*Client, error) {
	if cfg.Timeout == 0 {
		cfg.Timeout = 15 * time.Second
	}
	if cfg.Port == 0 {
		cfg.Port = 22
	}

	clientCfg, err := buildClientConfig(cfg)
	if err != nil {
		return nil, err
	}

	addr := joinHostPort(cfg.Address, cfg.Port)

	var conn *xssh.Client
	if cfg.Jumphost == nil {
		conn, err = dialDirect(ctx, addr, clientCfg)
	} else {
		jump, jerr := Dial(ctx, *cfg.Jumphost)
		if jerr != nil {
			return nil, fmt.Errorf("jumphost dial: %w", jerr)
		}
		conn, err = dialThrough(ctx, jump.conn, addr, clientCfg)
		if err != nil {
			jump.Close()
		}
	}
	if err != nil {
		return nil, fmt.Errorf("ssh dial %s: %w", addr, err)
	}
	return &Client{cfg: cfg, conn: conn}, nil
}

func (c *Client) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// Result captures the outcome of a single command execution.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
	Err      error
}

// OK returns true if the command exited 0 with no transport error.
func (r Result) OK() bool { return r.Err == nil && r.ExitCode == 0 }

// Run executes cmd on the remote host. The remote shell is whatever the
// account's default is; cmd is passed as a single argument to ssh.
func (c *Client) Run(ctx context.Context, cmd string) Result {
	sess, err := c.conn.NewSession()
	if err != nil {
		return Result{Err: fmt.Errorf("new session: %w", err)}
	}
	defer sess.Close()

	var stdout, stderr bytes.Buffer
	sess.Stdout = &stdout
	sess.Stderr = &stderr

	done := make(chan error, 1)
	go func() { done <- sess.Run(cmd) }()

	select {
	case <-ctx.Done():
		_ = sess.Signal(xssh.SIGKILL)
		return Result{Stdout: stdout.String(), Stderr: stderr.String(), Err: ctx.Err()}
	case runErr := <-done:
		r := Result{Stdout: stdout.String(), Stderr: stderr.String()}
		if runErr == nil {
			return r
		}
		var exitErr *xssh.ExitError
		if errors.As(runErr, &exitErr) {
			r.ExitCode = exitErr.ExitStatus()
			return r
		}
		r.Err = runErr
		return r
	}
}

// buildClientConfig wires authentication and host-key callback.
func buildClientConfig(cfg Config) (*xssh.ClientConfig, error) {
	if cfg.User == "" {
		return nil, errors.New("ssh: user is required")
	}
	if cfg.KeyPath == "" {
		return nil, errors.New("ssh: key_path is required (password auth not supported)")
	}
	keyBytes, err := os.ReadFile(cfg.KeyPath)
	if err != nil {
		return nil, fmt.Errorf("ssh: read key %s: %w", cfg.KeyPath, err)
	}
	signer, err := xssh.ParsePrivateKey(keyBytes)
	if err != nil {
		return nil, fmt.Errorf("ssh: parse key %s: %w", cfg.KeyPath, err)
	}

	hostKey, err := hostKeyCallback(cfg)
	if err != nil {
		return nil, err
	}

	return &xssh.ClientConfig{
		User:            cfg.User,
		Auth:            []xssh.AuthMethod{xssh.PublicKeys(signer)},
		HostKeyCallback: hostKey,
		Timeout:         cfg.Timeout,
	}, nil
}

// hostKeyCallback returns a TOFU known_hosts callback. On first contact
// with a host the key is appended; subsequent connections must match.
// If StrictHostKey is true and known_hosts has no entry, the connection is
// rejected. This keeps PoC ergonomics while not silently trusting forever.
func hostKeyCallback(cfg Config) (xssh.HostKeyCallback, error) {
	if cfg.KnownHosts == "" {
		// No known_hosts requested — accept any (for true one-shot use).
		return xssh.InsecureIgnoreHostKey(), nil //nolint:gosec // explicit opt-in
	}
	if err := os.MkdirAll(filepath.Dir(cfg.KnownHosts), 0o755); err != nil {
		return nil, err
	}
	if _, err := os.Stat(cfg.KnownHosts); errors.Is(err, os.ErrNotExist) {
		f, ferr := os.OpenFile(cfg.KnownHosts, os.O_CREATE|os.O_WRONLY, 0o644)
		if ferr != nil {
			return nil, ferr
		}
		_ = f.Close()
	}
	verify, err := knownhosts.New(cfg.KnownHosts)
	if err != nil {
		return nil, fmt.Errorf("ssh: load known_hosts: %w", err)
	}
	return func(host string, remote net.Addr, key xssh.PublicKey) error {
		err := verify(host, remote, key)
		if err == nil {
			return nil
		}
		var keyErr *knownhosts.KeyError
		if errors.As(err, &keyErr) && len(keyErr.Want) == 0 {
			// Unknown host — TOFU append unless strict.
			if cfg.StrictHostKey {
				return fmt.Errorf("ssh: host %s not in known_hosts (strict mode)", host)
			}
			return appendKnownHost(cfg.KnownHosts, host, remote, key)
		}
		return err
	}, nil
}

func appendKnownHost(path, host string, remote net.Addr, key xssh.PublicKey) error {
	line := knownhosts.Line([]string{knownhosts.Normalize(host), knownhosts.Normalize(remote.String())}, key)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintln(f, line)
	return err
}

func dialDirect(ctx context.Context, addr string, cfg *xssh.ClientConfig) (*xssh.Client, error) {
	d := net.Dialer{Timeout: cfg.Timeout}
	tcp, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	conn, chans, reqs, err := xssh.NewClientConn(tcp, addr, cfg)
	if err != nil {
		_ = tcp.Close()
		return nil, err
	}
	return xssh.NewClient(conn, chans, reqs), nil
}

func dialThrough(ctx context.Context, via *xssh.Client, addr string, cfg *xssh.ClientConfig) (*xssh.Client, error) {
	tcp, err := via.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	conn, chans, reqs, err := xssh.NewClientConn(tcp, addr, cfg)
	if err != nil {
		_ = tcp.Close()
		return nil, err
	}
	return xssh.NewClient(conn, chans, reqs), nil
}

func joinHostPort(addr string, port int) string {
	if _, _, err := net.SplitHostPort(addr); err == nil {
		return addr
	}
	return fmt.Sprintf("%s:%d", addr, port)
}
