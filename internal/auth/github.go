package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// GitHubProvider logs users in with a GitHub OAuth App. The user's access
// token is used only to read their profile and org memberships during login
// and is not stored.
type GitHubProvider struct {
	ClientID     string
	ClientSecret string
	RedirectURL  string // <base>/auth/callback
	ReadOrgs     bool   // request read:org to check org membership

	// Endpoints (overridable for tests).
	AuthorizeURL string
	TokenURL     string
	APIURL       string
	HTTP         *http.Client
}

func NewGitHubProvider(clientID, secret, redirectURL string, readOrgs bool) *GitHubProvider {
	return &GitHubProvider{
		ClientID: clientID, ClientSecret: secret, RedirectURL: redirectURL, ReadOrgs: readOrgs,
		AuthorizeURL: "https://github.com/login/oauth/authorize",
		TokenURL:     "https://github.com/login/oauth/access_token",
		APIURL:       "https://api.github.com",
		HTTP:         &http.Client{Timeout: 15 * time.Second},
	}
}

func (g *GitHubProvider) AuthCodeURL(state, loginHint string) string {
	q := url.Values{"client_id": {g.ClientID}, "redirect_uri": {g.RedirectURL}, "state": {state}, "allow_signup": {"false"}}
	if g.ReadOrgs {
		q.Set("scope", "read:org")
	}
	if loginHint != "" {
		q.Set("login", loginHint)
	}
	return g.AuthorizeURL + "?" + q.Encode()
}

func (g *GitHubProvider) Exchange(ctx context.Context, code string) (*User, error) {
	form := url.Values{"client_id": {g.ClientID}, "client_secret": {g.ClientSecret}, "code": {code}, "redirect_uri": {g.RedirectURL}}
	req, _ := http.NewRequestWithContext(ctx, "POST", g.TokenURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	var tok struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if err := g.do(req, &tok); err != nil {
		return nil, err
	}
	if tok.AccessToken == "" {
		return nil, fmt.Errorf("token exchange: %s %s", tok.Error, tok.Description)
	}
	var u struct {
		Login     string `json:"login"`
		Name      string `json:"name"`
		AvatarURL string `json:"avatar_url"`
	}
	if err := g.api(ctx, tok.AccessToken, "/user", &u); err != nil {
		return nil, err
	}
	user := &User{Login: u.Login, Name: u.Name, AvatarURL: u.AvatarURL}
	if user.Name == "" {
		user.Name = u.Login
	}
	if g.ReadOrgs {
		var orgs []struct {
			Login string `json:"login"`
		}
		if err := g.api(ctx, tok.AccessToken, "/user/orgs?per_page=100", &orgs); err != nil {
			return nil, err
		}
		for _, o := range orgs {
			user.Orgs = append(user.Orgs, o.Login)
		}
	}
	return user, nil
}

func (g *GitHubProvider) api(ctx context.Context, token, path string, out any) error {
	req, _ := http.NewRequestWithContext(ctx, "GET", g.APIURL+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	return g.do(req, out)
}

func (g *GitHubProvider) do(req *http.Request, out any) error {
	resp, err := g.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("%s %s: %d %s", req.Method, req.URL.Path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
