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
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

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
func NewClient(cfg Config) *Client {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // self-signed cert
	}
	return &Client{
		BaseURL: strings.TrimRight(cfg.URL, "/"),
		HTTP:    &http.Client{Transport: tr, Timeout: 30 * time.Second},
	}
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

// EnsureRunning checks the local bnk-forge listener. If healthy,
// returns immediately. Otherwise runs `make deploy` in the configured
// RepoPath, then polls the health endpoint until ready or timeout
// fires.
func EnsureRunning(ctx context.Context, cfg Config, out io.Writer) error {
	cli := NewClient(cfg)

	// Fast path: already up.
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := cli.Health(probeCtx); err == nil {
		fmt.Fprintf(out, "  bnk-forge already running at %s — skipping make deploy.\n", cfg.URL)
		return nil
	}

	// Sanity-check the repo path before kicking off make.
	if _, err := os.Stat(filepath.Join(cfg.RepoPath, "Makefile")); err != nil {
		return fmt.Errorf("bnk-forge Makefile not found at %s — clone https://github.com/sp-prod-field/bnk-forge to that path or override bnk_forge.repo_path",
			cfg.RepoPath)
	}

	fmt.Fprintf(out, "  bnk-forge not responding at %s — running `make deploy` in %s ...\n",
		cfg.URL, cfg.RepoPath)
	cmd := exec.CommandContext(ctx, "make", "deploy")
	cmd.Dir = cfg.RepoPath
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("make deploy in %s: %w", cfg.RepoPath, err)
	}

	// Poll for ready.
	fmt.Fprintln(out, "  Waiting for bnk-forge /api/system/health ...")
	deadline := time.Now().Add(3 * time.Minute)
	for {
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := cli.Health(probeCtx)
		cancel()
		if err == nil {
			fmt.Fprintln(out, "  bnk-forge is up.")
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("bnk-forge did not become healthy within 3m at %s (last error: %v)", cfg.URL, err)
		}
		time.Sleep(5 * time.Second)
	}
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
