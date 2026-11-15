// Package policy renders the per-session nono proxy profile.
//
// Security model:
//   - The sandbox has no route to the internet; the nono proxy is its only egress.
//   - Real credentials live only in the proxy container's environment and are
//     injected into requests that match a credential route.
//   - GitHub writes are limited to the session repository by endpoint rules.
//     Branch-level protection (e.g. "no pushes to main") must come from GitHub
//     rulesets because nono does not inspect git pack contents.
package policy

import (
	"encoding/json"
	"strings"
)

// Env var names read by nono (env:// credential sources). They are set on the
// proxy container only.
const (
	EnvGitHubToken = "REMODEV_GITHUB_TOKEN"
	EnvGitHubBasic = "REMODEV_GITHUB_BASIC"
	EnvAnthropic   = "REMODEV_ANTHROPIC_CREDENTIAL"
)

type Input struct {
	SessionID      string
	Repo           string // owner/repo, write access is limited to this repository
	NetworkProfile string // nono preset, e.g. "developer"
	AllowDomains   []string
	AllowGraphQL   bool
	AnthropicOAuth bool // true: Authorization: Bearer (Claude subscription token), false: x-api-key
}

type rule struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Reason string `json:"reason,omitempty"`
}

type endpointPolicy struct {
	Deny    []rule            `json:"deny,omitempty"`
	Allow   []rule            `json:"allow,omitempty"`
	Default map[string]string `json:"default"`
}

type credential struct {
	Upstream         string          `json:"upstream"`
	CredentialKey    string          `json:"credential_key"`
	InjectMode       string          `json:"inject_mode,omitempty"`
	InjectHeader     string          `json:"inject_header,omitempty"`
	CredentialFormat string          `json:"credential_format,omitempty"`
	EnvVar           string          `json:"env_var,omitempty"`
	PhantomFormat    string          `json:"phantom_format,omitempty"`
	EndpointPolicy   *endpointPolicy `json:"endpoint_policy,omitempty"`
}

func methods(ms []string, path string) []rule {
	out := make([]rule, 0, len(ms))
	for _, m := range ms {
		out = append(out, rule{Method: m, Path: path})
	}
	return out
}

// Render returns the nono profile JSON.
func Render(in Input) ([]byte, error) {
	repo := strings.ToLower(in.Repo)
	write := []string{"POST", "PATCH", "PUT"}

	api := &endpointPolicy{
		Deny: []rule{
			{Method: "*", Path: "/repos/*/*/actions/secrets/**", Reason: "secrets"},
			{Method: "*", Path: "/repos/*/*/hooks/**", Reason: "webhooks"},
			{Method: "*", Path: "/repos/*/*/keys/**", Reason: "deploy keys"},
			{Method: "*", Path: "/repos/*/*/collaborators/**", Reason: "collaborators"},
			{Method: "*", Path: "/repos/*/*/branches/*/protection/**", Reason: "branch protection"},
			{Method: "*", Path: "/repos/*/*/rulesets/**", Reason: "rulesets"},
			{Method: "DELETE", Path: "/**", Reason: "no deletes"},
		},
		Allow:   append(methods([]string{"GET", "HEAD"}, "/**"), methods(write, "/repos/"+repo+"/issues/**")...),
		Default: map[string]string{"decision": "deny"},
	}
	api.Allow = append(api.Allow, methods(write, "/repos/"+repo+"/pulls")...)
	api.Allow = append(api.Allow, methods(write, "/repos/"+repo+"/pulls/**")...)
	api.Allow = append(api.Allow, methods(write, "/repos/"+repo+"/statuses/**")...)
	if in.AllowGraphQL {
		api.Allow = append(api.Allow, rule{Method: "POST", Path: "/graphql"})
	}

	git := &endpointPolicy{
		Allow: []rule{
			{Method: "GET", Path: "/**"},
			{Method: "POST", Path: "/*/*/git-upload-pack"},
			{Method: "POST", Path: "/" + repo + "/git-receive-pack"},
			{Method: "POST", Path: "/" + repo + ".git/git-receive-pack"},
		},
		Default: map[string]string{"decision": "deny"},
	}

	anth := credential{
		Upstream:      "https://api.anthropic.com",
		CredentialKey: "env://" + EnvAnthropic,
		InjectHeader:  "x-api-key",
		EnvVar:        "ANTHROPIC_API_KEY",
		// format "{}" == raw key
		CredentialFormat: "{}",
	}
	if in.AnthropicOAuth {
		anth.InjectHeader = "Authorization"
		anth.CredentialFormat = "Bearer {}"
		anth.EnvVar = "CLAUDE_CODE_OAUTH_TOKEN"
		anth.PhantomFormat = "sk-ant-oat01-{}"
	}

	profile := map[string]any{
		"meta": map[string]any{"name": "remodev-" + in.SessionID},
		"network": map[string]any{
			"network_profile": in.NetworkProfile,
			"allow_domain":    nonNil(in.AllowDomains),
			"credentials":     []string{"anthropic_api", "github_api", "github_git"},
			"custom_credentials": map[string]credential{
				"anthropic_api": anth,
				"github_api": {
					Upstream:         "https://api.github.com",
					CredentialKey:    "env://" + EnvGitHubToken,
					InjectHeader:     "Authorization",
					CredentialFormat: "Bearer {}",
					EnvVar:           "GH_TOKEN",
					PhantomFormat:    "ghp_{}",
					EndpointPolicy:   api,
				},
				"github_git": {
					Upstream:       "https://github.com",
					CredentialKey:  "env://" + EnvGitHubBasic,
					InjectMode:     "basic_auth",
					EnvVar:         "REMODEV_GIT_PHANTOM",
					EndpointPolicy: git,
				},
			},
		},
	}
	return json.MarshalIndent(profile, "", "  ")
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
