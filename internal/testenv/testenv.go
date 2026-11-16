// Package testenv assembles a control plane with fake dependencies for tests.
package testenv

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/naotama2002/remodev/internal/auth"
	"github.com/naotama2002/remodev/internal/ca"
	"github.com/naotama2002/remodev/internal/config"
	"github.com/naotama2002/remodev/internal/fake"
	"github.com/naotama2002/remodev/internal/server"
	"github.com/naotama2002/remodev/internal/session"
	"github.com/naotama2002/remodev/internal/store"
)

type Env struct {
	T      *testing.T
	Auth   *auth.Auth
	Cfg    *config.Config
	Store  *store.Store
	RT     *fake.Runtime
	GH     *fake.GitHub
	M      *session.Manager
	Server *httptest.Server
}

// Option configures the test environment.
type Option func(*options)

type options struct{ auth bool }

// WithAuth enables login (fake GitHub provider). Members of org "acme" are
// allowed; logins starting with "outsider" belong to no org.
func WithAuth() Option { return func(o *options) { o.auth = true } }

// New returns a running control plane backed by fakes. Allowed repos: acme/*.
// The default repo "acme/app" exists on the fake GitHub.
func New(t *testing.T, opts ...Option) *Env {
	var o options
	for _, f := range opts {
		f(&o)
	}
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{
		BaseURL:          "http://remodev.test",
		DataDir:          dir,
		Runtime:          "fake",
		AllowedRepos:     []string{"acme/*"},
		PreviewDomain:    "preview.remodev.test",
		IdleStop:         30 * time.Minute,
		Retain:           7 * 24 * time.Hour,
		Agents:           []config.Agent{{Name: "default", Description: "default"}},
		GitHubToken:      "real-github-token",
		ClaudeOAuthToken: "real-claude-token",
	}
	st, err := store.Open(filepath.Join(dir, "db"))
	if err != nil {
		t.Fatal(err)
	}
	authority, err := ca.LoadOrCreate(filepath.Join(dir, "ca"))
	if err != nil {
		t.Fatal(err)
	}
	gh := fake.NewGitHub("acme/app")
	rt := fake.NewRuntime()
	m := &session.Manager{Cfg: cfg, Store: st, RT: rt, CA: authority, GH: gh,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), ArtifactsDir: filepath.Join(dir, "artifacts")}
	m.Init(context.Background())
	t.Cleanup(m.Close) // registered after TempDir, so it runs before the dir is removed
	a := &auth.Auth{Mode: "none", Key: []byte("test-key-test-key-test-key-12345"), BaseURL: cfg.BaseURL}
	if o.auth {
		a.Mode, a.Provider, a.AllowedOrgs = "github", &auth.FakeProvider{Org: "acme"}, []string{"acme"}
	}
	srv := httptest.NewServer((&server.Server{Cfg: cfg, M: m, Auth: a}).Handler())
	t.Cleanup(srv.Close)
	return &Env{T: t, Auth: a, Cfg: cfg, Store: st, RT: rt, GH: gh, M: m, Server: srv}
}

// WaitIdle waits until the session has no turn running or queued.
func (e *Env) WaitIdle(id string) store.Session {
	e.T.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s, ok := e.Store.Get(id); ok && !e.M.Busy(id) && s.Status != store.StatusStarting && s.Status != store.StatusRunning {
			return s
		}
		time.Sleep(10 * time.Millisecond)
	}
	s, _ := e.Store.Get(id)
	e.T.Fatalf("session %s did not become idle (status %s)", id, s.Status)
	return s
}

// EventsOf returns the events of the given types.
func (e *Env) EventsOf(id string, types ...string) []store.Event {
	e.T.Helper()
	evs, err := e.Store.Events(id, 0)
	if err != nil {
		e.T.Fatal(err)
	}
	want := map[string]bool{}
	for _, t := range types {
		want[t] = true
	}
	var out []store.Event
	for _, ev := range evs {
		if len(types) == 0 || want[ev.Type] {
			out = append(out, ev)
		}
	}
	return out
}

// Client is an HTTP client with its own cookie jar (one browser).
type Client struct {
	T    *testing.T
	Base string
	HTTP *http.Client
}

// NewClient returns an anonymous client.
func (e *Env) NewClient() *Client {
	jar, _ := cookiejar.New(nil)
	return &Client{T: e.T, Base: e.Server.URL, HTTP: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}}
}

// Login logs in as login through the OAuth flow and returns the client.
func (e *Env) Login(login string) *Client {
	e.T.Helper()
	c := e.NewClient()
	resp, _ := c.Do("GET", "/auth/login", "")
	state := ""
	if u, err := url.Parse(resp.Header.Get("Location")); err == nil {
		state = u.Query().Get("state")
	}
	resp, body := c.Do("GET", "/auth/callback?"+url.Values{"state": {state}, "code": {login}}.Encode(), "")
	if resp.StatusCode != http.StatusFound {
		e.T.Fatalf("login %s: %d %s", login, resp.StatusCode, body)
	}
	return c
}

// Do sends a request; body (if any) is sent as JSON.
func (c *Client) Do(method, path, body string) (*http.Response, []byte) {
	c.T.Helper()
	req, _ := http.NewRequest(method, c.Base+path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		c.T.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, b
}
