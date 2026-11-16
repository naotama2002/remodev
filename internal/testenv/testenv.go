// Package testenv assembles a control plane with fake dependencies for tests.
package testenv

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/naotama2002/remodev/internal/ca"
	"github.com/naotama2002/remodev/internal/config"
	"github.com/naotama2002/remodev/internal/fake"
	"github.com/naotama2002/remodev/internal/server"
	"github.com/naotama2002/remodev/internal/session"
	"github.com/naotama2002/remodev/internal/store"
)

type Env struct {
	T      *testing.T
	Cfg    *config.Config
	Store  *store.Store
	RT     *fake.Runtime
	GH     *fake.GitHub
	M      *session.Manager
	Server *httptest.Server
}

// New returns a running control plane backed by fakes. Allowed repos: acme/*.
// The default repo "acme/app" exists on the fake GitHub.
func New(t *testing.T) *Env {
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
	srv := httptest.NewServer((&server.Server{Cfg: cfg, M: m}).Handler())
	t.Cleanup(srv.Close)
	return &Env{T: t, Cfg: cfg, Store: st, RT: rt, GH: gh, M: m, Server: srv}
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
