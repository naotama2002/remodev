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

	"github.com/naotama2002/remodev/internal/ca"
	"github.com/naotama2002/remodev/internal/config"
	"github.com/naotama2002/remodev/internal/docker"
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
	rt := &session.DockerRuntime{
		D:             docker.New(cfg.DockerSock),
		EgressNetwork: cfg.EgressNetwork,
		Control:       cfg.ControlContainer,
		OCIRuntime:    cfg.SandboxRuntime,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	m := &session.Manager{Cfg: cfg, Store: st, RT: rt, CA: authority, GH: github.New(cfg.GitHubToken), Log: log,
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
	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           (&server.Server{Cfg: cfg, M: m, UI: ui, Log: log}).Handler(),
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
	return nil
}
