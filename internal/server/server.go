// Package server exposes the HTTP API, the SSE event stream, sandbox previews
// and the embedded web UI.
package server

import (
	"context"
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

	"github.com/naotama2002/remodev/internal/auth"
	"github.com/naotama2002/remodev/internal/config"
	"github.com/naotama2002/remodev/internal/session"
	"github.com/naotama2002/remodev/internal/store"
)

type Server struct {
	Cfg  *config.Config
	M    *session.Manager
	Auth *auth.Auth
	UI   fs.FS // built web UI (may be nil)
	Log  *slog.Logger
	// FakeLogin serves the development login page (REMODEV_RUNTIME=fake only).
	FakeLogin http.HandlerFunc
}

// SessionView is the API representation of a session for one user.
type SessionView struct {
	store.Session
	Busy        bool           `json:"busy"`
	PreviewBase string         `json:"previewBase"` // e.g. http://p{port}-<id>.preview.localhost:8080
	Access      session.Access `json:"access"`
}

func (s *Server) scheme() string {
	if strings.HasPrefix(s.Cfg.BaseURL, "https://") {
		return "https"
	}
	return "http"
}

func (s *Server) view(x store.Session, u *auth.User) SessionView {
	if x.Visibility == "" {
		x.Visibility = session.VisibilityTeam
	}
	return SessionView{Session: x, Busy: s.M.Busy(x.ID), Access: session.AccessFor(x, u.Login),
		PreviewBase: fmt.Sprintf("%s://p{port}-%s.%s", s.scheme(), x.ID, s.Cfg.PreviewDomain)}
}

type need int

const (
	needView need = iota
	needWrite
	needDelete
	needOwner
)

// load returns the session at {id} if the user may perform the action.
// Sessions the user cannot see are reported as 404, not 403, so their
// existence is not revealed.
func (s *Server) load(w http.ResponseWriter, r *http.Request, n need) (store.Session, *auth.User, bool) {
	u := userOf(r)
	x, ok := s.M.Store.Get(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, session.ErrNotFound)
		return x, u, false
	}
	a := session.AccessFor(x, u.Login)
	if !a.View {
		writeErr(w, 404, session.ErrNotFound)
		return x, u, false
	}
	allowed := map[need]bool{needView: true, needWrite: a.Write, needDelete: a.Delete, needOwner: a.ChangeVisibility}[n]
	if !allowed {
		writeErr(w, 403, errors.New("only the creator of the session can do this"))
		return x, u, false
	}
	return x, u, true
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
	api.HandleFunc("PATCH /api/sessions/{id}", s.updateSession)
	api.HandleFunc("POST /api/logout", s.Auth.Logout)

	public := http.NewServeMux()
	public.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	public.HandleFunc("GET /auth/login", s.Auth.Login)
	public.HandleFunc("GET /auth/callback", s.Auth.Callback)
	public.HandleFunc("GET /auth/preview", s.previewLogin)
	if s.FakeLogin != nil {
		public.HandleFunc("GET /auth/fake", s.FakeLogin)
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
		if h, pattern := public.Handler(r); pattern != "" {
			h.ServeHTTP(w, r)
			return
		}
		u, ok := s.Auth.UserFrom(r)
		if !ok {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				writeErr(w, 401, errors.New("login required"))
				return
			}
			http.Redirect(w, r, "/auth/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), userKey{}, u))
		if s.UI != nil && !strings.HasPrefix(r.URL.Path, "/api/") {
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			spa(s.UI).ServeHTTP(w, r)
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

type userKey struct{}

func userOf(r *http.Request) *auth.User {
	if u, ok := r.Context().Value(userKey{}).(*auth.User); ok {
		return u
	}
	return &auth.User{}
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
	writeJSON(w, 200, map[string]any{"allowedRepos": s.Cfg.AllowedRepos, "agents": agents, "user": userOf(r), "authEnabled": s.Auth.Enabled()})
}

func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	list := s.M.Store.List()
	out := make([]SessionView, 0, len(list))
	for _, x := range list {
		if session.AccessFor(x, u.Login).View {
			out = append(out, s.view(x, u))
		}
	}
	writeJSON(w, 200, out)
}

func (s *Server) getSession(w http.ResponseWriter, r *http.Request) {
	x, u, ok := s.load(w, r, needView)
	if ok {
		writeJSON(w, 200, s.view(x, u))
	}
}

func (s *Server) updateSession(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Visibility string `json:"visibility"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err)
		return
	}
	x, u, ok := s.load(w, r, needOwner)
	if !ok {
		return
	}
	x, err := s.M.SetVisibility(x.ID, req.Visibility, u.Login)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, s.view(x, u))
}

func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	var req session.CreateRequest
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err)
		return
	}
	u := userOf(r)
	req.CreatedBy, req.Source = u.Login, "web"
	x, err := s.M.Create(r.Context(), req)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 201, s.view(x, u))
}

func (s *Server) sendMessage(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text string `json:"text"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err)
		return
	}
	x, u, ok := s.load(w, r, needWrite)
	if !ok {
		return
	}
	err := s.M.Send(x.ID, req.Text, u.Login)
	switch {
	case err != nil:
		writeErr(w, 400, err)
	default:
		writeJSON(w, 202, map[string]bool{"queued": true})
	}
}

func (s *Server) interrupt(w http.ResponseWriter, r *http.Request) {
	x, _, ok := s.load(w, r, needWrite)
	if !ok {
		return
	}
	s.M.Interrupt(r.Context(), x.ID)
	writeJSON(w, 202, map[string]bool{"ok": true})
}

func (s *Server) stop(w http.ResponseWriter, r *http.Request) {
	x, _, ok := s.load(w, r, needWrite)
	if !ok {
		return
	}
	if err := s.M.Stop(r.Context(), x.ID); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) deleteSession(w http.ResponseWriter, r *http.Request) {
	x, _, ok := s.load(w, r, needDelete)
	if !ok {
		return
	}
	if err := s.M.Delete(r.Context(), x.ID); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) listArtifacts(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.load(w, r, needView); !ok {
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
	if _, _, ok := s.load(w, r, needView); !ok {
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
	if _, _, ok := s.load(w, r, needView); !ok {
		return
	}
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
	if _, _, ok := s.load(w, r, needView); !ok {
		return
	}
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
	x, _, ok := s.load(w, r, needView)
	if !ok {
		return
	}
	id := x.ID
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
	if r.URL.Path == "/__remodev/auth" {
		s.Auth.RedeemPreview(w, r, id)
		return
	}
	login, ok := s.Auth.PreviewUser(r, id)
	if !ok {
		if r.Method == http.MethodGet {
			http.Redirect(w, r, s.Auth.PreviewLoginURL(id, port, r.URL.RequestURI()), http.StatusFound)
		} else {
			http.Error(w, "login required", http.StatusUnauthorized)
		}
		return
	}
	x, ok := s.M.Store.Get(id)
	if !ok || !session.AccessFor(x, login).View {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if x.Status == store.StatusStopped || x.Status == store.StatusDeleted {
		http.Error(w, "session is not running", http.StatusNotFound)
		return
	}
	target := &url.URL{Scheme: "http", Host: s.M.RT.PreviewTarget(id, port)}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = "localhost:" + strconv.Itoa(port) // dev servers often check Host
			pr.SetXForwarded()
			auth.StripCookies(pr.Out) // never hand remodev cookies to the sandbox
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, fmt.Sprintf("nothing is listening on port %d in the sandbox yet (%v)", port, err), http.StatusBadGateway)
		},
	}
	rp.ServeHTTP(w, r)
}

// previewLogin runs on the main origin: it checks the user may view the
// session and sends the browser to the preview host with a one-time ticket.
func (s *Server) previewLogin(w http.ResponseWriter, r *http.Request) {
	u, ok := s.Auth.UserFrom(r)
	if !ok {
		http.Redirect(w, r, "/auth/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
		return
	}
	q := r.URL.Query()
	id := q.Get("session")
	port, err := strconv.Atoi(q.Get("port"))
	x, found := s.M.Store.Get(id)
	if err != nil || port < 1 || port > 65535 || !found || !session.AccessFor(x, u.Login).View {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	host := fmt.Sprintf("p%d-%s.%s", port, id, s.Cfg.PreviewDomain)
	t := s.Auth.PreviewTicket(u, id, host, q.Get("next"))
	http.Redirect(w, r, fmt.Sprintf("%s://%s/__remodev/auth?ticket=%s", s.scheme(), host, url.QueryEscape(t)), http.StatusFound)
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
