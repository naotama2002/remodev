// Package server exposes the HTTP API, the SSE event stream, sandbox previews
// and the embedded web UI.
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/naotama2002/remodev/internal/config"
	"github.com/naotama2002/remodev/internal/session"
	"github.com/naotama2002/remodev/internal/store"
)

type Server struct {
	Cfg *config.Config
	M   *session.Manager
	UI  fs.FS // built web UI (may be nil)
	Log *slog.Logger
}

// SessionView is the API representation of a session.
type SessionView struct {
	store.Session
	Busy        bool   `json:"busy"`
	PreviewBase string `json:"previewBase"` // e.g. http://p{port}-<id>.preview.localhost:8080
}

func (s *Server) view(x store.Session) SessionView {
	scheme := "http"
	if strings.HasPrefix(s.Cfg.BaseURL, "https://") {
		scheme = "https"
	}
	return SessionView{Session: x, Busy: s.M.Busy(x.ID),
		PreviewBase: fmt.Sprintf("%s://p{port}-%s.%s", scheme, x.ID, s.Cfg.PreviewDomain)}
}

func (s *Server) Handler() http.Handler {
	api := http.NewServeMux()
	api.HandleFunc("GET /api/config", s.getConfig)
	api.HandleFunc("GET /api/sessions", s.listSessions)
	api.HandleFunc("POST /api/sessions", s.createSession)
	api.HandleFunc("GET /api/sessions/{id}", s.getSession)
	api.HandleFunc("POST /api/sessions/{id}/messages", s.sendMessage)
	api.HandleFunc("POST /api/sessions/{id}/interrupt", s.interrupt)
	api.HandleFunc("POST /api/sessions/{id}/stop", s.stop)
	api.HandleFunc("DELETE /api/sessions/{id}", s.deleteSession)
	api.HandleFunc("GET /api/sessions/{id}/events", s.events)
	api.HandleFunc("GET /api/sessions/{id}/artifacts", s.listArtifacts)
	api.HandleFunc("GET /api/sessions/{id}/artifacts/{name}", s.getArtifact)
	api.HandleFunc("GET /api/sessions/{id}/diff", s.diff)
	api.HandleFunc("GET /api/sessions/{id}/checks", s.checks)
	api.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	if s.UI != nil {
		api.Handle("GET /", spa(s.UI))
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ip := remoteIP(r); ip != nil && s.M.FromSandbox(ip) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if id, port, ok := s.previewHost(r.Host); ok {
			s.preview(w, r, id, port)
			return
		}
		// Basic CSRF protection for the cookie-less local mode and for future
		// cookie auth: state-changing requests must be JSON (forces CORS preflight).
		if r.Method != http.MethodGet && r.Method != http.MethodHead && strings.HasPrefix(r.URL.Path, "/api/") {
			if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
				http.Error(w, "content-type must be application/json", http.StatusUnsupportedMediaType)
				return
			}
		}
		api.ServeHTTP(w, r)
	})
}

func remoteIP(r *http.Request) net.IP {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return nil
	}
	return net.ParseIP(host)
}

// ---- JSON helpers -------------------------------------------------------------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func readJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func user(r *http.Request) string {
	if u := r.Header.Get("X-Remodev-User"); u != "" { // set by an auth proxy (e.g. ALB OIDC)
		return u
	}
	return "local"
}

// ---- handlers -------------------------------------------------------------------

func (s *Server) getConfig(w http.ResponseWriter, r *http.Request) {
	type agent struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	var agents []agent
	for _, a := range s.Cfg.Agents {
		agents = append(agents, agent{a.Name, a.Description})
	}
	writeJSON(w, 200, map[string]any{"allowedRepos": s.Cfg.AllowedRepos, "agents": agents, "user": user(r)})
}

func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	list := s.M.Store.List()
	out := make([]SessionView, 0, len(list))
	for _, x := range list {
		out = append(out, s.view(x))
	}
	writeJSON(w, 200, out)
}

func (s *Server) getSession(w http.ResponseWriter, r *http.Request) {
	x, ok := s.M.Store.Get(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, session.ErrNotFound)
		return
	}
	writeJSON(w, 200, s.view(x))
}

func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	var req session.CreateRequest
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err)
		return
	}
	req.CreatedBy, req.Source = user(r), "web"
	x, err := s.M.Create(r.Context(), req)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 201, s.view(x))
}

func (s *Server) sendMessage(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text string `json:"text"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err)
		return
	}
	err := s.M.Send(r.PathValue("id"), req.Text, user(r))
	switch {
	case errors.Is(err, session.ErrNotFound):
		writeErr(w, 404, err)
	case err != nil:
		writeErr(w, 400, err)
	default:
		writeJSON(w, 202, map[string]bool{"queued": true})
	}
}

func (s *Server) interrupt(w http.ResponseWriter, r *http.Request) {
	s.M.Interrupt(r.Context(), r.PathValue("id"))
	writeJSON(w, 202, map[string]bool{"ok": true})
}

func (s *Server) stop(w http.ResponseWriter, r *http.Request) {
	if err := s.M.Stop(r.Context(), r.PathValue("id")); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) deleteSession(w http.ResponseWriter, r *http.Request) {
	if err := s.M.Delete(r.Context(), r.PathValue("id")); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) listArtifacts(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.M.Store.Get(r.PathValue("id")); !ok {
		writeErr(w, 404, session.ErrNotFound)
		return
	}
	list, err := s.M.Artifacts(r.PathValue("id"))
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, list)
}

// getArtifact serves a stored artifact. Artifacts are produced by the agent
// and therefore untrusted: they are sandboxed with CSP so HTML cannot run
// script against the control plane origin.
func (s *Server) getArtifact(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.M.Store.Get(r.PathValue("id")); !ok {
		writeErr(w, 404, session.ErrNotFound)
		return
	}
	p, err := s.M.ArtifactPath(r.PathValue("id"), r.PathValue("name"))
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	h := w.Header()
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'; img-src data: 'self'; style-src 'unsafe-inline'; media-src 'self'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "private, max-age=60")
	if r.URL.Query().Get("download") == "1" {
		h.Set("Content-Disposition", "attachment")
	}
	http.ServeFile(w, r, p)
}

func (s *Server) diff(w http.ResponseWriter, r *http.Request) {
	d, err := s.M.Diff(r.Context(), r.PathValue("id"))
	if errors.Is(err, session.ErrNotFound) {
		writeErr(w, 404, err)
		return
	}
	if err != nil {
		writeErr(w, 502, err)
		return
	}
	writeJSON(w, 200, d)
}

func (s *Server) checks(w http.ResponseWriter, r *http.Request) {
	c, err := s.M.Checks(r.Context(), r.PathValue("id"))
	if errors.Is(err, session.ErrNotFound) {
		writeErr(w, 404, err)
		return
	}
	if err != nil {
		writeErr(w, 502, err)
		return
	}
	writeJSON(w, 200, c)
}

// events streams the session log as Server-Sent Events. Clients resume with
// Last-Event-ID (EventSource does this automatically) or ?after=<seq>.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.M.Store.Get(id); !ok {
		writeErr(w, 404, session.ErrNotFound)
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		after, _ = strconv.ParseInt(v, 10, 64)
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, errors.New("streaming unsupported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	ch, cancel := s.M.Store.Subscribe(id) // subscribe first so nothing is missed
	defer cancel()
	backlog, err := s.M.Store.Events(id, after)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	send := func(ev store.Event) {
		if ev.Seq <= after {
			return
		}
		after = ev.Seq
		b, _ := json.Marshal(ev)
		fmt.Fprintf(w, "id: %d\nevent: message\ndata: %s\n\n", ev.Seq, b)
	}
	for _, ev := range backlog {
		send(ev)
	}
	fmt.Fprint(w, "event: ready\ndata: {}\n\n")
	fl.Flush()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-ch:
			send(ev)
			fl.Flush()
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		}
	}
}

// ---- previews ---------------------------------------------------------------------

var previewRe = regexp.MustCompile(`^p(\d{2,5})-([0-9a-f]{10})\.`)

func (s *Server) previewHost(host string) (id string, port int, ok bool) {
	if !strings.HasSuffix(host, "."+s.Cfg.PreviewDomain) {
		return "", 0, false
	}
	m := previewRe.FindStringSubmatch(host)
	if m == nil {
		return "", 0, false
	}
	port, _ = strconv.Atoi(m[1])
	if port < 1 || port > 65535 {
		return "", 0, false
	}
	return m[2], port, true
}

func (s *Server) preview(w http.ResponseWriter, r *http.Request, id string, port int) {
	x, ok := s.M.Store.Get(id)
	if !ok || x.Status == store.StatusStopped || x.Status == store.StatusDeleted {
		http.Error(w, "session is not running", http.StatusNotFound)
		return
	}
	target := &url.URL{Scheme: "http", Host: s.M.RT.PreviewTarget(id, port)}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = "localhost:" + strconv.Itoa(port) // dev servers often check Host
			pr.SetXForwarded()
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, fmt.Sprintf("nothing is listening on port %d in the sandbox yet (%v)", port, err), http.StatusBadGateway)
		},
	}
	rp.ServeHTTP(w, r)
}

// spa serves the built UI and falls back to index.html for client routes.
func spa(ui fs.FS) http.Handler {
	files := http.FileServerFS(ui)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p != "" {
			if _, err := fs.Stat(ui, p); err == nil {
				if strings.HasPrefix(p, "assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				files.ServeHTTP(w, r)
				return
			}
		}
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFileFS(w, r, ui, "index.html")
	})
}
