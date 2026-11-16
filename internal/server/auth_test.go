package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/naotama2002/remodev/internal/testenv"
)

func createAs(t *testing.T, c *testenv.Client, visibility string) apiSession {
	t.Helper()
	body := `{"repo":"acme/app","prompt":"hello"`
	if visibility != "" {
		body += `,"visibility":"` + visibility + `"`
	}
	resp, b := c.Do("POST", "/api/sessions", body+"}")
	if resp.StatusCode != 201 {
		t.Fatalf("create: %d %s", resp.StatusCode, b)
	}
	var s apiSession
	_ = json.Unmarshal(b, &s)
	return s
}

func TestLoginIsRequired(t *testing.T) {
	e := testenv.New(t, testenv.WithAuth())
	anon := e.NewClient()
	if resp, _ := anon.Do("GET", "/api/sessions", ""); resp.StatusCode != 401 {
		t.Fatalf("api without login = %d", resp.StatusCode)
	}
	resp, _ := anon.Do("GET", "/s/abc?x=1", "")
	if resp.StatusCode != 302 || resp.Header.Get("Location") != "/auth/login?next=%2Fs%2Fabc%3Fx%3D1" {
		t.Fatalf("page without login = %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp, _ := anon.Do("GET", "/healthz", ""); resp.StatusCode != 200 {
		t.Fatal("healthz must stay public")
	}
}

func TestLoginRecordsTheUserAsCreator(t *testing.T) {
	e := testenv.New(t, testenv.WithAuth())
	alice := e.Login("alice")
	_, b := alice.Do("GET", "/api/config", "")
	if !strings.Contains(string(b), `"login":"alice"`) || !strings.Contains(string(b), `"authEnabled":true`) {
		t.Fatalf("config: %s", b)
	}
	s := createAs(t, alice, "")
	got, _ := e.Store.Get(s.ID)
	if got.CreatedBy != "alice" || got.Visibility != "team" {
		t.Fatalf("session: %+v", got)
	}
	// The request body cannot spoof the creator.
	resp, _ := alice.Do("POST", "/api/sessions", `{"repo":"acme/app","prompt":"x","createdBy":"mallory"}`)
	if resp.StatusCode != 400 {
		t.Fatalf("unknown field accepted: %d", resp.StatusCode)
	}
}

func TestUsersOutsideTheOrgCannotLogIn(t *testing.T) {
	e := testenv.New(t, testenv.WithAuth())
	c := e.NewClient()
	resp, _ := c.Do("GET", "/auth/login", "")
	u, _ := url.Parse(resp.Header.Get("Location"))
	resp, _ = c.Do("GET", "/auth/callback?state="+u.Query().Get("state")+"&code=outsider", "")
	if resp.StatusCode != 403 {
		t.Fatalf("outsider login = %d", resp.StatusCode)
	}
	if resp, _ := c.Do("GET", "/api/sessions", ""); resp.StatusCode != 401 {
		t.Fatal("outsider got a session")
	}
}

func TestOAuthStateMustMatch(t *testing.T) {
	e := testenv.New(t, testenv.WithAuth())
	c := e.NewClient()
	c.Do("GET", "/auth/login", "")
	if resp, _ := c.Do("GET", "/auth/callback?state=forged&code=alice", ""); resp.StatusCode != 400 {
		t.Fatalf("forged state = %d", resp.StatusCode)
	}
	// Without the state cookie (e.g. attacker-initiated callback).
	if resp, _ := e.NewClient().Do("GET", "/auth/callback?state=x&code=alice", ""); resp.StatusCode != 400 {
		t.Fatalf("no state cookie = %d", resp.StatusCode)
	}
}

func TestLoginRedirectsOnlyToLocalPaths(t *testing.T) {
	e := testenv.New(t, testenv.WithAuth())
	for next, want := range map[string]string{"/s/abc": "/s/abc", "https://evil.example/": "/", "//evil.example/": "/"} {
		c := e.NewClient()
		resp, _ := c.Do("GET", "/auth/login?next="+url.QueryEscape(next), "")
		u, _ := url.Parse(resp.Header.Get("Location"))
		resp, _ = c.Do("GET", "/auth/callback?state="+u.Query().Get("state")+"&code=alice", "")
		if got := resp.Header.Get("Location"); got != want {
			t.Errorf("next=%q redirected to %q, want %q", next, got, want)
		}
	}
}

func TestTeamSessionsAreSharedButOnlyTheCreatorDeletes(t *testing.T) {
	e := testenv.New(t, testenv.WithAuth())
	alice, bob := e.Login("alice"), e.Login("bob")
	s := createAs(t, alice, "")
	e.WaitIdle(s.ID)

	_, b := bob.Do("GET", "/api/sessions", "")
	if !strings.Contains(string(b), s.ID) {
		t.Fatal("team session not listed for teammate")
	}
	if !strings.Contains(string(b), `"delete":false`) {
		t.Fatalf("teammate access should not include delete: %s", b)
	}
	if resp, _ := bob.Do("POST", "/api/sessions/"+s.ID+"/messages", `{"text":"from bob"}`); resp.StatusCode != 202 {
		t.Fatalf("teammate message = %d", resp.StatusCode)
	}
	e.WaitIdle(s.ID)
	if last := e.EventsOf(s.ID, "user"); last[len(last)-1].Text != "from bob" || !strings.Contains(string(last[len(last)-1].Data), `"bob"`) {
		t.Fatal("message author not recorded")
	}
	if resp, _ := bob.Do("DELETE", "/api/sessions/"+s.ID, `{}`); resp.StatusCode != 403 {
		t.Fatalf("teammate delete = %d", resp.StatusCode)
	}
	if resp, _ := bob.Do("PATCH", "/api/sessions/"+s.ID, `{"visibility":"private"}`); resp.StatusCode != 403 {
		t.Fatalf("teammate visibility change = %d", resp.StatusCode)
	}
	if resp, _ := alice.Do("DELETE", "/api/sessions/"+s.ID, `{}`); resp.StatusCode != 200 {
		t.Fatalf("creator delete = %d", resp.StatusCode)
	}
}

func TestPrivateSessionsAreInvisibleToOthers(t *testing.T) {
	e := testenv.New(t, testenv.WithAuth())
	alice, bob := e.Login("alice"), e.Login("bob")
	s := createAs(t, alice, "private")
	e.WaitIdle(s.ID)

	if _, b := bob.Do("GET", "/api/sessions", ""); strings.Contains(string(b), s.ID) {
		t.Fatal("private session listed for another user")
	}
	for _, p := range []string{"", "/events", "/artifacts", "/diff", "/checks"} {
		if resp, _ := bob.Do("GET", "/api/sessions/"+s.ID+p, ""); resp.StatusCode != 404 {
			t.Errorf("GET %s by other user = %d, want 404", p, resp.StatusCode)
		}
	}
	if resp, _ := bob.Do("POST", "/api/sessions/"+s.ID+"/messages", `{"text":"x"}`); resp.StatusCode != 404 {
		t.Fatalf("message by other user = %d", resp.StatusCode)
	}
	if resp, _ := alice.Do("GET", "/api/sessions/"+s.ID, ""); resp.StatusCode != 200 {
		t.Fatal("creator cannot see own private session")
	}
	// The creator can share it with the team.
	if resp, _ := alice.Do("PATCH", "/api/sessions/"+s.ID, `{"visibility":"team"}`); resp.StatusCode != 200 {
		t.Fatal("make team")
	}
	if resp, _ := bob.Do("GET", "/api/sessions/"+s.ID, ""); resp.StatusCode != 200 {
		t.Fatal("shared session not visible")
	}
}

func TestPreviewRequiresAccessAndNeverForwardsRemodevCookies(t *testing.T) {
	e := testenv.New(t, testenv.WithAuth())
	var gotCookies string
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCookies = r.Header.Get("Cookie")
		w.Write([]byte("dev server"))
	}))
	defer app.Close()
	u, _ := url.Parse(app.URL)
	e.RT.Preview = u.Host

	alice, bob := e.Login("alice"), e.Login("bob")
	s := createAs(t, alice, "private")
	e.WaitIdle(s.ID)
	host := "p3000-" + s.ID + ".preview.remodev.test"

	// open drives the redirect chain main origin -> preview host by hand
	// (the test server has one address for both hosts).
	open := func(c *testenv.Client) (int, string) {
		get := func(host, path string) *http.Response {
			req, _ := http.NewRequest("GET", e.Server.URL+path, nil)
			req.Host = host
			req.Header.Set("Cookie", "app=1")
			for _, ck := range c.HTTP.Jar.Cookies(&url.URL{Scheme: "http", Host: hostOnly(host)}) {
				req.AddCookie(ck)
			}
			resp, err := c.HTTP.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			c.HTTP.Jar.SetCookies(&url.URL{Scheme: "http", Host: hostOnly(host)}, resp.Cookies())
			return resp
		}
		main := strings.TrimPrefix(e.Server.URL, "http://")
		cur := host
		resp := get(cur, "/page")
		for i := 0; i < 5 && resp.StatusCode == 302; i++ {
			loc, _ := url.Parse(resp.Header.Get("Location"))
			switch {
			case loc.Host == "": // relative: same host
			case loc.Host == "remodev.test": // BaseURL = main origin
				cur = main
			default:
				cur = loc.Host
			}
			resp = get(cur, loc.RequestURI())
		}
		buf := make([]byte, 64)
		n, _ := resp.Body.Read(buf)
		resp.Body.Close()
		return resp.StatusCode, string(buf[:n])
	}
	if code, body := open(alice); code != 200 || body != "dev server" {
		t.Fatalf("creator preview = %d %q", code, body)
	}
	if strings.Contains(gotCookies, "remodev_") || !strings.Contains(gotCookies, "app=1") {
		t.Fatalf("cookies forwarded to sandbox: %q", gotCookies)
	}
	if code, _ := open(bob); code != 404 {
		t.Fatalf("other user preview of private session = %d", code)
	}
	if _, body := open(e.NewClient()); body == "dev server" {
		t.Fatal("anonymous user opened a preview")
	}
}

func hostOnly(h string) string {
	if i := strings.LastIndex(h, ":"); i > 0 && !strings.Contains(h[i:], "]") {
		return h[:i]
	}
	return h
}

// A second login attempt (another tab, or /favicon.ico redirected to login)
// must not break one that is in progress.
func TestParallelLoginAttemptsDoNotInterfere(t *testing.T) {
	e := testenv.New(t, testenv.WithAuth())
	c := e.NewClient()
	start := func() string {
		resp, _ := c.Do("GET", "/auth/login", "")
		u, _ := url.Parse(resp.Header.Get("Location"))
		return u.Query().Get("state")
	}
	first := start()
	_ = start() // e.g. the browser fetching /favicon.ico before login
	resp, b := c.Do("GET", "/auth/callback?state="+first+"&code=alice", "")
	if resp.StatusCode != 302 {
		t.Fatalf("first login attempt broken by the second: %d %s", resp.StatusCode, b)
	}
}
