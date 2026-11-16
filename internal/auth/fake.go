package auth

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// FakeProvider is a login page that accepts any user name. It is only
// available with REMODEV_RUNTIME=fake (UI development and E2E tests).
// Users whose login starts with "outsider" belong to no organization.
type FakeProvider struct {
	Org string // organization every other user belongs to
}

func (f *FakeProvider) AuthCodeURL(state, loginHint string) string {
	return "/auth/fake?" + url.Values{"state": {state}, "login": {loginHint}}.Encode()
}

func (f *FakeProvider) Exchange(_ context.Context, code string) (*User, error) {
	login := strings.TrimSpace(code)
	if login == "" || strings.ContainsAny(login, " /<>\"'") {
		return nil, errors.New("invalid user name")
	}
	u := &User{Login: login, Name: login}
	if !strings.HasPrefix(login, "outsider") {
		u.Orgs = []string{f.Org}
	}
	return u, nil
}

// Page renders the fake login form; it submits to /auth/callback.
func (f *FakeProvider) Page(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>remodev fake login</title>
<body style="font-family:sans-serif;padding:40px">
<h1>remodev (fake login)</h1>
<p>REMODEV_RUNTIME=fake のための開発用ログインです。任意のユーザー名でログインできます。</p>
<form action="/auth/callback" method="get">
<input type="hidden" name="state" value="%s">
<label>ユーザー名 <input name="code" value="%s" autofocus></label>
<button type="submit">ログイン</button>
</form>`, html.EscapeString(q.Get("state")), html.EscapeString(q.Get("login")))
}

// LoadKey reads the cookie signing key from path, creating it if missing.
func LoadKey(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err == nil && len(b) >= 32 {
		return b, nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	b = make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return b, os.WriteFile(path, b, 0o600)
}
