// Package auth implements login (GitHub OAuth), signed session cookies and
// the preview handshake that lets a logged-in user open a sandbox preview on
// its own host without sharing the main session cookie with the sandbox.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

type User struct {
	Login     string   `json:"login"`
	Name      string   `json:"name"`
	AvatarURL string   `json:"avatarUrl"`
	Orgs      []string `json:"-"`
}

// Provider is an OAuth identity provider (GitHub, or a fake in tests).
type Provider interface {
	// AuthCodeURL is where the browser is sent to log in. loginHint suggests an account.
	AuthCodeURL(state, loginHint string) string
	// Exchange turns the callback code into a user (including org memberships).
	Exchange(ctx context.Context, code string) (*User, error)
}

const (
	SessionCookie = "remodev_session"
	StateCookie   = "remodev_oauth"
	PreviewCookie = "remodev_preview"

	sessionTTL = 7 * 24 * time.Hour
	stateTTL   = 10 * time.Minute
	ticketTTL  = time.Minute
	previewTTL = 12 * time.Hour
)

// LocalUser is the identity used when authentication is disabled.
var LocalUser = &User{Login: "local", Name: "local"}

type Auth struct {
	Mode         string // none | github | fake
	Provider     Provider
	Key          []byte // HMAC key for cookies
	AllowedOrgs  []string
	AllowedUsers []string
	BaseURL      string
	Log          *slog.Logger
	Now          func() time.Time
}

func (a *Auth) Enabled() bool { return a.Mode != "" && a.Mode != "none" }

func (a *Auth) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *Auth) secure() bool { return strings.HasPrefix(a.BaseURL, "https://") }

// Allowed reports whether u may use remodev.
func (a *Auth) Allowed(u *User) bool {
	for _, x := range a.AllowedUsers {
		if strings.EqualFold(x, u.Login) {
			return true
		}
	}
	for _, o := range u.Orgs {
		if slices.ContainsFunc(a.AllowedOrgs, func(x string) bool { return strings.EqualFold(x, o) }) {
			return true
		}
	}
	return false
}

// ---- signed tokens -------------------------------------------------------------

type claims struct {
	Kind    string `json:"k"`
	Login   string `json:"l,omitempty"`
	Name    string `json:"n,omitempty"`
	Avatar  string `json:"a,omitempty"`
	Session string `json:"s,omitempty"` // remodev session id (previews)
	Host    string `json:"h,omitempty"` // preview host (tickets)
	State   string `json:"st,omitempty"`
	Next    string `json:"nx,omitempty"`
	Exp     int64  `json:"e"`
}

var errInvalid = errors.New("invalid or expired token")

func (a *Auth) sign(c claims, ttl time.Duration) string {
	c.Exp = a.now().Add(ttl).Unix()
	b, _ := json.Marshal(c)
	p := base64.RawURLEncoding.EncodeToString(b)
	m := hmac.New(sha256.New, a.Key)
	m.Write([]byte(p))
	return p + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (a *Auth) verify(tok, kind string) (*claims, error) {
	p, sig, ok := strings.Cut(tok, ".")
	if !ok {
		return nil, errInvalid
	}
	m := hmac.New(sha256.New, a.Key)
	m.Write([]byte(p))
	want := m.Sum(nil)
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, want) {
		return nil, errInvalid
	}
	b, err := base64.RawURLEncoding.DecodeString(p)
	if err != nil {
		return nil, errInvalid
	}
	var c claims
	if json.Unmarshal(b, &c) != nil || c.Kind != kind || a.now().Unix() > c.Exp {
		return nil, errInvalid
	}
	return &c, nil
}

func (a *Auth) setCookie(w http.ResponseWriter, name, value, path string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: value, Path: path, MaxAge: int(ttl.Seconds()),
		HttpOnly: true, Secure: a.secure(), SameSite: http.SameSiteLaxMode,
	})
}

func (a *Auth) clearCookie(w http.ResponseWriter, name, path string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: path, MaxAge: -1, HttpOnly: true, Secure: a.secure(), SameSite: http.SameSiteLaxMode})
}

// SafeNext returns p if it is a same-origin path, otherwise "/".
func SafeNext(p string) string {
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.HasPrefix(p, "/\\") || strings.ContainsAny(p, "\r\n") {
		return "/"
	}
	return p
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ---- login flow ------------------------------------------------------------------

// UserFrom returns the logged-in user. With auth disabled everyone is LocalUser.
func (a *Auth) UserFrom(r *http.Request) (*User, bool) {
	if !a.Enabled() {
		return LocalUser, true
	}
	ck, err := r.Cookie(SessionCookie)
	if err != nil {
		return nil, false
	}
	c, err := a.verify(ck.Value, "session")
	if err != nil {
		return nil, false
	}
	return &User{Login: c.Login, Name: c.Name, AvatarURL: c.Avatar}, true
}

// Login starts the OAuth flow. Query: next (path to return to), login (account hint).
func (a *Auth) Login(w http.ResponseWriter, r *http.Request) {
	if !a.Enabled() {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	state := randHex(16)
	next := SafeNext(r.URL.Query().Get("next"))
	// One cookie per login attempt: a parallel attempt (another tab, or a
	// subresource such as /favicon.ico redirected to login) must not
	// overwrite the state of this one.
	a.setCookie(w, StateCookie+"_"+state, a.sign(claims{Kind: "state", State: state, Next: next}, stateTTL), "/auth", stateTTL)
	http.Redirect(w, r, a.Provider.AuthCodeURL(state, r.URL.Query().Get("login")), http.StatusFound)
}

// Callback completes the OAuth flow and sets the session cookie.
func (a *Auth) Callback(w http.ResponseWriter, r *http.Request) {
	if !a.Enabled() {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	q := r.URL.Query().Get("state")
	if len(q) != 32 || strings.Trim(q, "0123456789abcdef") != "" {
		page(w, http.StatusBadRequest, "ログインの状態が一致しません。もう一度ログインしてください。")
		return
	}
	ck, err := r.Cookie(StateCookie + "_" + q)
	if err != nil {
		page(w, http.StatusBadRequest, "ログインの有効期限が切れました。もう一度ログインしてください。")
		return
	}
	st, err := a.verify(ck.Value, "state")
	if err != nil || subtle.ConstantTimeCompare([]byte(st.State), []byte(q)) != 1 {
		page(w, http.StatusBadRequest, "ログインの状態が一致しません。もう一度ログインしてください。")
		return
	}
	a.clearCookie(w, StateCookie+"_"+q, "/auth")
	if e := r.URL.Query().Get("error"); e != "" {
		page(w, http.StatusForbidden, "ログインがキャンセルされました: "+e)
		return
	}
	u, err := a.Provider.Exchange(r.Context(), r.URL.Query().Get("code"))
	if err != nil {
		if a.Log != nil {
			a.Log.Warn("oauth exchange failed", "err", err)
		}
		page(w, http.StatusBadGateway, "GitHub での認証に失敗しました。")
		return
	}
	if !a.Allowed(u) {
		if a.Log != nil {
			a.Log.Warn("login denied", "login", u.Login)
		}
		page(w, http.StatusForbidden, fmt.Sprintf("%s には remodev を使う権限がありません（許可された organization のメンバーではありません）。", u.Login))
		return
	}
	a.setCookie(w, SessionCookie, a.sign(claims{Kind: "session", Login: u.Login, Name: u.Name, Avatar: u.AvatarURL}, sessionTTL), "/", sessionTTL)
	http.Redirect(w, r, st.Next, http.StatusFound)
}

func (a *Auth) Logout(w http.ResponseWriter, r *http.Request) {
	a.clearCookie(w, SessionCookie, "/")
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}

func page(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>remodev</title><body style="font-family:sans-serif;padding:40px"><h1>remodev</h1><p>%s</p><p><a href="/auth/login">ログインし直す</a></p>`, html.EscapeString(msg))
}

// ---- previews --------------------------------------------------------------------
//
// Previews live on their own host (p<port>-<session>.<preview domain>), which
// is served by the sandbox's dev server, so the main session cookie must never
// be sent there. Instead the main origin issues a short-lived ticket bound to
// (user, session, host); the preview host redeems it for a host-only cookie
// that grants access to that one preview, and strips the cookie before
// forwarding requests to the sandbox.

// PreviewTicket returns a one-minute ticket for opening a preview host.
func (a *Auth) PreviewTicket(u *User, sessionID, host, next string) string {
	return a.sign(claims{Kind: "ticket", Login: u.Login, Session: sessionID, Host: host, Next: SafeNext(next)}, ticketTTL)
}

// RedeemPreview handles /__remodev/auth on a preview host.
func (a *Auth) RedeemPreview(w http.ResponseWriter, r *http.Request, sessionID string) {
	c, err := a.verify(r.URL.Query().Get("ticket"), "ticket")
	if err != nil || c.Session != sessionID || !strings.EqualFold(c.Host, r.Host) {
		http.Error(w, "invalid preview ticket", http.StatusForbidden)
		return
	}
	a.setCookie(w, PreviewCookie, a.sign(claims{Kind: "preview", Login: c.Login, Session: sessionID}, previewTTL), "/", previewTTL)
	http.Redirect(w, r, c.Next, http.StatusFound)
}

// PreviewUser returns the login allowed to view this session's preview.
func (a *Auth) PreviewUser(r *http.Request, sessionID string) (string, bool) {
	if !a.Enabled() {
		return LocalUser.Login, true
	}
	ck, err := r.Cookie(PreviewCookie)
	if err != nil {
		return "", false
	}
	c, err := a.verify(ck.Value, "preview")
	if err != nil || c.Session != sessionID {
		return "", false
	}
	return c.Login, true
}

// StripCookies removes remodev cookies from a request before it is proxied
// to the sandbox.
func StripCookies(r *http.Request) {
	cookies := r.Cookies()
	r.Header.Del("Cookie")
	for _, c := range cookies {
		if c.Name == PreviewCookie || c.Name == SessionCookie || strings.HasPrefix(c.Name, StateCookie) {
			continue
		}
		r.AddCookie(c)
	}
}

// PreviewLoginURL is where an unauthenticated preview request is sent.
func (a *Auth) PreviewLoginURL(sessionID string, port int, next string) string {
	q := url.Values{"session": {sessionID}, "port": {fmt.Sprint(port)}, "next": {SafeNext(next)}}
	return a.BaseURL + "/auth/preview?" + q.Encode()
}
