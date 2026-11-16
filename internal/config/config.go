// Package config loads remodev settings from environment variables.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"
	"time"
)

type Agent struct {
	Name               string   `json:"name"`
	Description        string   `json:"description"`
	Model              string   `json:"model,omitempty"`
	AppendSystemPrompt string   `json:"appendSystemPrompt,omitempty"`
	AllowDomains       []string `json:"allowDomains,omitempty"`
}

type Config struct {
	Listen     string
	BaseURL    string // public URL of the UI, e.g. http://localhost:8080
	DataDir    string
	DockerSock string
	// Runtime: docker (default) or fake (in-memory sandbox, GitHub and Claude;
	// for UI development and end-to-end tests, needs no credentials).
	Runtime string

	// Docker runtime
	SandboxImage     string
	ProxyImage       string
	EgressNetwork    string // network with internet access (proxy side)
	ControlContainer string // our own container name, joined to session networks for previews
	SandboxRuntime   string // optional OCI runtime, e.g. runsc (gVisor)
	SandboxMemory    int64
	SandboxCPUs      float64

	// Credentials (never enter the sandbox)
	GitHubToken       string
	AnthropicAPIKey   string
	ClaudeOAuthToken  string
	AllowedRepos      []string // glob patterns: owner/*, owner/repo
	AllowGraphQL      bool
	NetworkProfile    string // nono network profile for the sandbox
	ExtraAllowDomains []string

	// Previews: p<port>-<session>.<PreviewDomain>
	PreviewDomain string

	// Lifecycle
	IdleStop time.Duration
	Retain   time.Duration

	Agents []Agent
}

func env(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func list(k string) []string {
	var out []string
	for _, s := range strings.Split(os.Getenv(k), ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func Load() (*Config, error) {
	c := &Config{
		Listen:            env("REMODEV_LISTEN", ":8080"),
		BaseURL:           strings.TrimRight(env("REMODEV_BASE_URL", "http://localhost:8080"), "/"),
		DataDir:           env("REMODEV_DATA_DIR", "./data"),
		DockerSock:        env("REMODEV_DOCKER_SOCK", "/var/run/docker.sock"),
		Runtime:           env("REMODEV_RUNTIME", "docker"),
		SandboxImage:      env("REMODEV_SANDBOX_IMAGE", "remodev-sandbox:dev"),
		ProxyImage:        env("REMODEV_PROXY_IMAGE", "remodev-nono:dev"),
		EgressNetwork:     env("REMODEV_EGRESS_NETWORK", "remodev-egress"),
		ControlContainer:  env("REMODEV_CONTROL_CONTAINER", ""),
		SandboxRuntime:    env("REMODEV_SANDBOX_RUNTIME", ""),
		GitHubToken:       env("REMODEV_GITHUB_TOKEN", ""),
		AnthropicAPIKey:   env("REMODEV_ANTHROPIC_API_KEY", ""),
		ClaudeOAuthToken:  env("REMODEV_CLAUDE_OAUTH_TOKEN", ""),
		AllowedRepos:      list("REMODEV_ALLOWED_REPOS"),
		AllowGraphQL:      env("REMODEV_ALLOW_GRAPHQL", "false") == "true",
		NetworkProfile:    env("REMODEV_NETWORK_PROFILE", "developer"),
		ExtraAllowDomains: list("REMODEV_ALLOW_DOMAINS"),
		PreviewDomain:     env("REMODEV_PREVIEW_DOMAIN", "preview.localhost:8080"),
	}
	var err error
	if c.SandboxMemory, err = strconv.ParseInt(env("REMODEV_SANDBOX_MEMORY_MB", "4096"), 10, 64); err != nil {
		return nil, fmt.Errorf("REMODEV_SANDBOX_MEMORY_MB: %w", err)
	}
	c.SandboxMemory <<= 20
	if c.SandboxCPUs, err = strconv.ParseFloat(env("REMODEV_SANDBOX_CPUS", "2"), 64); err != nil {
		return nil, fmt.Errorf("REMODEV_SANDBOX_CPUS: %w", err)
	}
	if c.IdleStop, err = time.ParseDuration(env("REMODEV_IDLE_STOP", "30m")); err != nil {
		return nil, fmt.Errorf("REMODEV_IDLE_STOP: %w", err)
	}
	if c.Retain, err = time.ParseDuration(env("REMODEV_RETAIN", "168h")); err != nil {
		return nil, fmt.Errorf("REMODEV_RETAIN: %w", err)
	}
	switch c.Runtime {
	case "docker":
		if c.GitHubToken == "" {
			return nil, fmt.Errorf("REMODEV_GITHUB_TOKEN is required")
		}
		if c.AnthropicAPIKey == "" && c.ClaudeOAuthToken == "" {
			return nil, fmt.Errorf("REMODEV_ANTHROPIC_API_KEY or REMODEV_CLAUDE_OAUTH_TOKEN is required")
		}
	case "fake":
		if c.AnthropicAPIKey == "" && c.ClaudeOAuthToken == "" {
			c.AnthropicAPIKey = "fake"
		}
	default:
		return nil, fmt.Errorf("REMODEV_RUNTIME must be docker or fake")
	}
	if len(c.AllowedRepos) == 0 {
		return nil, fmt.Errorf("REMODEV_ALLOWED_REPOS is required (e.g. myorg/*)")
	}
	c.Agents = []Agent{{Name: "default", Description: "Claude Code with the default settings"}}
	if f := env("REMODEV_AGENTS_FILE", ""); f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("agents file: %w", err)
		}
		var extra []Agent
		if err := json.Unmarshal(b, &extra); err != nil {
			return nil, fmt.Errorf("agents file: %w", err)
		}
		for _, a := range extra {
			if a.Name == "default" {
				c.Agents[0] = a
			} else {
				c.Agents = append(c.Agents, a)
			}
		}
	}
	return c, nil
}

// RepoAllowed reports whether owner/repo matches REMODEV_ALLOWED_REPOS.
func (c *Config) RepoAllowed(repo string) bool {
	repo = strings.ToLower(repo)
	if strings.Count(repo, "/") != 1 {
		return false
	}
	for _, p := range c.AllowedRepos {
		if ok, _ := path.Match(strings.ToLower(p), repo); ok {
			return true
		}
	}
	return false
}

func (c *Config) Agent(name string) (Agent, bool) {
	if name == "" {
		name = "default"
	}
	for _, a := range c.Agents {
		if a.Name == name {
			return a, true
		}
	}
	return Agent{}, false
}
