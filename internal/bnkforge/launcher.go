// Package bnkforge integrates a local bnk-forge installation
// (https://github.com/sp-prod-field/bnk-forge — currently private) with
// a dpubnkctl PoC: bring the local stack up if it isn't already, then
// POST a project to its API that mirrors the PoC's metadata so the
// operator can drive Day-2 work in bnk-forge against the same cluster.
//
// Scope intentionally small: this package shells to `make` in the
// bnk-forge clone, polls the health endpoint, and makes two HTTP calls
// (login, create project). Anything more (uploading kubeconfig as a
// project credential, wiring the cluster into a bnk-forge module) is
// future work.
package bnkforge

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ErrNotRunning is the sentinel returned by RequireRunning when the
// bnk-forge backend is not responding at cfg.URL. Callers that want to
// soft-skip the integration (e.g. `cluster up`'s auto-hook) check for
// this with errors.Is; explicit operator invocations (`dpubnkctl
// bnk-forge launch`) propagate it as a regular error.
//
// dpubnkctl never installs bnk-forge for the operator: if the stack
// isn't running, that's a deliberate operator decision (or oversight)
// — surface it cleanly, don't shell out to `make deploy` in the
// background.
var ErrNotRunning = errors.New("bnk-forge is not running")

// Config is the operator-facing surface — sourced from poc.yaml's
// bnk_forge: block, with defaults filled in.
type Config struct {
	RepoPath      string
	URL           string // e.g. https://localhost
	AdminUsername string
	AdminPassword string
}

// WithDefaults fills in any zero fields from baked-in defaults.
func (c Config) WithDefaults() Config {
	if c.URL == "" {
		c.URL = "https://localhost"
	}
	if c.AdminUsername == "" {
		c.AdminUsername = "admin"
	}
	if c.AdminPassword == "" {
		c.AdminPassword = "changeme"
	}
	if c.RepoPath == "" {
		c.RepoPath = "~/git/bnk-forge"
	}
	c.RepoPath = expandHome(c.RepoPath)
	return c
}

// Project carries the fields we set when POST-ing to bnk-forge's
// /api/projects. Names match the upstream `ProjectCreate` schema; only
// the subset dpubnkctl populates is here.
type Project struct {
	Name                  string `json:"name"`
	Description           string `json:"description"`
	ProjectType           string `json:"project_type"`
	CloudProvider         string `json:"cloud_provider"`
	Environment           string `json:"environment"`
	Region                string `json:"region,omitempty"`
	TargetPlatformProfile string `json:"target_platform_profile"`
	Color                 string `json:"color,omitempty"`
	Icon                  string `json:"icon,omitempty"`
}

// Client is the small HTTP wrapper we need. TLS verification disabled
// because the bnk-forge proxy uses a self-signed cert by default
// (operator accepts it on first browser open; same posture here).
type Client struct {
	BaseURL string
	HTTP    *http.Client
	Token   string
}

// NewClient returns a Client configured to talk to the bnk-forge
// listener at cfg.URL with sane timeouts.
//
// TLS posture: bnk-forge defaults to self-signed certs on localhost so
// we skip verify when the URL host resolves to a loopback address.
// For non-loopback URLs (operator pointed poc.yaml at a shared
// bnk-forge on the lab network) we keep verify on — otherwise an on-
// path attacker can MITM the admin login and capture the cluster
// kubeconfig we POST during registration. Operators with a real-cert
// gap can supply a CA bundle via SSL_CERT_FILE.
func NewClient(cfg Config) *Client {
	tr := &http.Transport{}
	if isLoopbackURL(cfg.URL) {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // localhost self-signed
	}
	return &Client{
		BaseURL: strings.TrimRight(cfg.URL, "/"),
		HTTP:    &http.Client{Transport: tr, Timeout: 30 * time.Second},
	}
}

// isLoopbackURL returns true if rawURL points at 127.0.0.0/8, ::1, or
// the literal host "localhost". Used to gate InsecureSkipVerify so the
// "self-signed cert is fine" exemption only applies on-machine.
func isLoopbackURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Health hits /api/system/health. Returns nil if the listener answered
// with 2xx; error otherwise.
func (c *Client) Health(ctx context.Context) error {
	req, _ := http.NewRequestWithContext(ctx, "GET", c.BaseURL+"/api/system/health", nil)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("health %s", resp.Status)
	}
	return nil
}

// Login POSTs /api/auth/login and stores the token on the Client.
func (c *Client) Login(ctx context.Context, user, pass string) error {
	body, _ := json.Marshal(map[string]string{"username": user, "password": pass})
	req, _ := http.NewRequestWithContext(ctx, "POST",
		c.BaseURL+"/api/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("login %s: %s", resp.Status, truncate(string(raw), 200))
	}
	var out struct {
		Token              string `json:"token"`
		MustChangePassword bool   `json:"must_change_password"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("decode login response: %w", err)
	}
	if out.Token == "" {
		return errors.New("login returned no token")
	}
	c.Token = out.Token
	return nil
}

// FindProjectByName GETs /api/projects and scans for an existing
// project with the given name. Returns (id, true) if found,
// (0, false) if not.
func (c *Client) FindProjectByName(ctx context.Context, name string) (int, bool, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", c.BaseURL+"/api/projects", nil)
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return 0, false, fmt.Errorf("list projects %s", resp.Status)
	}
	var out struct {
		Projects []struct {
			ID   int    `json:"id"`
			Name string `json:"name"`
		} `json:"projects"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, false, err
	}
	for _, p := range out.Projects {
		if p.Name == name {
			return p.ID, true, nil
		}
	}
	return 0, false, nil
}

// Cluster mirrors bnk-forge's ClusterCreateRequest: a Kubernetes
// cluster the project should manage. kubeconfig is the base64-encoded
// YAML body of the localized kubeconfig dpubnkctl writes to
// artifacts/kubeconfig.
type Cluster struct {
	Name             string `json:"name"`
	Kubeconfig       string `json:"kubeconfig"` // base64-encoded YAML
	CloudProvider    string `json:"cloud_provider,omitempty"`
	Region           string `json:"region,omitempty"`
	Context          string `json:"context,omitempty"`
	DefaultNamespace string `json:"default_namespace,omitempty"`
}

// ListProjectClusters GETs /api/projects/{id}/k8s/clusters and returns
// the list. Used to check whether the cluster we're about to register
// already exists (idempotency).
func (c *Client) ListProjectClusters(ctx context.Context, projectID int) ([]struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}, error) {
	url := fmt.Sprintf("%s/api/projects/%d/k8s/clusters", c.BaseURL, projectID)
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("list project clusters %s: %s", resp.Status, truncate(string(raw), 200))
	}
	var out struct {
		Clusters []struct {
			ID   int    `json:"id"`
			Name string `json:"name"`
		} `json:"clusters"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Clusters, nil
}

// CreateProjectCluster POSTs /api/projects/{id}/k8s/clusters. Returns
// the new cluster's id.
func (c *Client) CreateProjectCluster(ctx context.Context, projectID int, k Cluster) (int, error) {
	url := fmt.Sprintf("%s/api/projects/%d/k8s/clusters", c.BaseURL, projectID)
	body, _ := json.Marshal(k)
	req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("create project cluster %s: %s", resp.Status, truncate(string(raw), 400))
	}
	var out struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, fmt.Errorf("decode cluster-create response: %w", err)
	}
	return out.ID, nil
}

// DeleteCluster removes a cluster registration from bnk-forge.
// Idempotent — a 404 (already gone) is not an error.
func (c *Client) DeleteCluster(ctx context.Context, clusterID int) error {
	url := fmt.Sprintf("%s/api/k8s/clusters/%d", c.BaseURL, clusterID)
	req, _ := http.NewRequestWithContext(ctx, "DELETE", url, nil)
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 || resp.StatusCode/100 == 2 {
		return nil
	}
	raw, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("delete cluster %d: %s: %s", clusterID, resp.Status, truncate(string(raw), 200))
}

// DeleteProject removes a project from bnk-forge. Idempotent — 404
// is not an error. The bnk-forge backend cascades downstream (deletes
// remaining clusters / variables / etc. for the project), but we
// still call DeleteCluster first so the cluster's row is gone before
// the parent project delete.
func (c *Client) DeleteProject(ctx context.Context, projectID int) error {
	url := fmt.Sprintf("%s/api/projects/%d", c.BaseURL, projectID)
	req, _ := http.NewRequestWithContext(ctx, "DELETE", url, nil)
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 || resp.StatusCode/100 == 2 {
		return nil
	}
	raw, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("delete project %d: %s: %s", projectID, resp.Status, truncate(string(raw), 200))
}

// CreateProject POSTs /api/projects with the given payload. Returns
// the new project's id.
func (c *Client) CreateProject(ctx context.Context, p Project) (int, error) {
	body, _ := json.Marshal(p)
	req, _ := http.NewRequestWithContext(ctx, "POST",
		c.BaseURL+"/api/projects", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("create project %s: %s", resp.Status, truncate(string(raw), 300))
	}
	var out struct {
		Success   bool   `json:"success"`
		ProjectID int    `json:"project_id"`
		Name      string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, fmt.Errorf("decode create response: %w", err)
	}
	if !out.Success || out.ProjectID == 0 {
		return 0, fmt.Errorf("create project returned success=false")
	}
	return out.ProjectID, nil
}

// RequireRunning probes the local bnk-forge listener. Returns nil
// when healthy. When unreachable, returns ErrNotRunning (wrapping the
// underlying transport error) — callers decide whether to soft-skip
// or hard-fail. We do NOT shell out to `make deploy`; if the stack
// isn't up, that's the operator's call to make.
func RequireRunning(ctx context.Context, cfg Config, out io.Writer) error {
	cli := NewClient(cfg)
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := cli.Health(probeCtx); err != nil {
		fmt.Fprintf(out, "  bnk-forge not responding at %s: %v\n", cfg.URL, err)
		return fmt.Errorf("%w at %s — start it manually (e.g. `cd %s && make deploy`) and retry",
			ErrNotRunning, cfg.URL, cfg.RepoPath)
	}
	fmt.Fprintf(out, "  bnk-forge is up at %s.\n", cfg.URL)
	return nil
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[2:])
		}
	}
	return p
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
