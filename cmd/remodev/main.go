// Command remodev is the control plane: API + SSE + previews + embedded UI.
package main

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/naotama2002/remodev/internal/auth"
	"github.com/naotama2002/remodev/internal/ca"
	"github.com/naotama2002/remodev/internal/config"
	"github.com/naotama2002/remodev/internal/docker"
	"github.com/naotama2002/remodev/internal/fake"
	"github.com/naotama2002/remodev/internal/github"
	"github.com/naotama2002/remodev/internal/server"
	"github.com/naotama2002/remodev/internal/session"
	"github.com/naotama2002/remodev/internal/store"
	"github.com/naotama2002/remodev/web"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(cfg.DataDir, "db"))
	if err != nil {
		return err
	}
	authority, err := ca.LoadOrCreate(filepath.Join(cfg.DataDir, "ca"))
	if err != nil {
		return err
	}
	var (
		rt session.Runtime
		gh session.GitHub
	)
	switch cfg.Runtime {
	case "fake":
		fgh := fake.NewGitHub()
		fgh.AnyRepo = true
		frt := fake.NewRuntime()
		frt.Claude = fake.DemoClaude(fgh)
		rt, gh = frt, fgh
		log.Warn("REMODEV_RUNTIME=fake: no sandboxes are created; Claude and GitHub are simulated")
	default:
		rt = &session.DockerRuntime{
			D:             docker.New(cfg.DockerSock),
			EgressNetwork: cfg.EgressNetwork,
			Control:       cfg.ControlContainer,
			OCIRuntime:    cfg.SandboxRuntime,
		}
		gh = github.New(cfg.GitHubToken)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	m := &session.Manager{Cfg: cfg, Store: st, RT: rt, CA: authority, GH: gh, Log: log,
		ArtifactsDir: filepath.Join(cfg.DataDir, "artifacts")}
	m.Init(ctx)

	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				m.Reap(ctx)
			}
		}
	}()

	var ui fs.FS
	if sub, err := fs.Sub(web.Dist, "dist"); err == nil {
		if _, err := fs.Stat(sub, "index.html"); err == nil {
			ui = sub
		}
	}
	key, err := auth.LoadKey(filepath.Join(cfg.DataDir, "auth.key"))
	if err != nil {
		return err
	}
	a := &auth.Auth{Mode: cfg.AuthMode, Key: key, AllowedOrgs: cfg.AllowedOrgs, AllowedUsers: cfg.AllowedUsers, BaseURL: cfg.BaseURL, Log: log}
	var fakeLogin http.HandlerFunc
	if cfg.AuthMode == "github" {
		if cfg.Runtime == "fake" && cfg.GitHubClientID == "" {
			org := "acme"
			if len(cfg.AllowedOrgs) > 0 {
				org = cfg.AllowedOrgs[0]
			}
			fp := &auth.FakeProvider{Org: org}
			a.Provider, fakeLogin = fp, fp.Page
			log.Warn("REMODEV_AUTH=github with REMODEV_RUNTIME=fake: using the fake login page")
		} else {
			a.Provider = auth.NewGitHubProvider(cfg.GitHubClientID, cfg.GitHubClientSecret, cfg.BaseURL+"/auth/callback", len(cfg.AllowedOrgs) > 0)
		}
	}
	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           (&server.Server{Cfg: cfg, M: m, Auth: a, UI: ui, Log: log, FakeLogin: fakeLogin}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	log.Info("listening", "addr", cfg.Listen, "ui", ui != nil)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	log.Info("shutting down: interrupting running turns")
	m.Close()
	return nil
}
