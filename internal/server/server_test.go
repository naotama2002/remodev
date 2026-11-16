package server_test

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/naotama2002/remodev/internal/fake"
	"github.com/naotama2002/remodev/internal/testenv"
)

type apiSession struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	PRURL       string `json:"prUrl"`
	PreviewBase string `json:"previewBase"`
}

func do(t *testing.T, e *testenv.Env, method, path, body string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, e.Server.URL+path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, b
}

func createSession(t *testing.T, e *testenv.Env) apiSession {
	t.Helper()
	resp, b := do(t, e, "POST", "/api/sessions", `{"repo":"acme/app","prompt":"hello"}`)
	if resp.StatusCode != 201 {
		t.Fatalf("create: %d %s", resp.StatusCode, b)
	}
	var s apiSession
	_ = json.Unmarshal(b, &s)
	return s
}

func TestSessionLifecycleOverHTTP(t *testing.T) {
	e := testenv.New(t)
	e.RT.Claude = fake.DemoClaude(e.GH)
	s := createSession(t, e)
	e.WaitIdle(s.ID)

	_, b := do(t, e, "GET", "/api/sessions/"+s.ID, "")
	var got apiSession
	_ = json.Unmarshal(b, &got)
	if got.Status != "idle" || got.PRURL == "" || !strings.Contains(got.PreviewBase, "{port}-"+s.ID) {
		t.Fatalf("get: %s", b)
	}
	if resp, _ := do(t, e, "POST", "/api/sessions/"+s.ID+"/messages", `{"text":"more"}`); resp.StatusCode != 202 {
		t.Fatalf("message: %d", resp.StatusCode)
	}
	e.WaitIdle(s.ID)

	_, b = do(t, e, "GET", "/api/sessions/"+s.ID+"/artifacts", "")
	var arts []struct{ Name string }
	_ = json.Unmarshal(b, &arts)
	if len(arts) != 1 {
		t.Fatalf("artifacts: %s", b)
	}
	resp, img := do(t, e, "GET", "/api/sessions/"+s.ID+"/artifacts/"+arts[0].Name, "")
	if resp.StatusCode != 200 || string(img) != string(fake.TinyPNG) {
		t.Fatalf("artifact: %d", resp.StatusCode)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.HasPrefix(csp, "sandbox") {
		t.Fatalf("artifact must be served sandboxed, CSP=%q", csp)
	}
	for _, p := range []string{"/diff", "/checks"} {
		if resp, b := do(t, e, "GET", "/api/sessions/"+s.ID+p, ""); resp.StatusCode != 200 {
			t.Fatalf("%s: %d %s", p, resp.StatusCode, b)
		}
	}
	if resp, _ := do(t, e, "POST", "/api/sessions/"+s.ID+"/stop", `{}`); resp.StatusCode != 200 {
		t.Fatal("stop")
	}
	if resp, _ := do(t, e, "DELETE", "/api/sessions/"+s.ID, `{}`); resp.StatusCode != 200 {
		t.Fatal("delete")
	}
	if resp, _ := do(t, e, "POST", "/api/sessions/"+s.ID+"/messages", `{"text":"x"}`); resp.StatusCode != 400 {
		t.Fatalf("message after delete: %d", resp.StatusCode)
	}
}

func TestAPIErrors(t *testing.T) {
	e := testenv.New(t)
	s := createSession(t, e)
	e.WaitIdle(s.ID)
	cases := []struct {
		method, path, body string
		want               int
	}{
		{"POST", "/api/sessions", `{"repo":"other/x","prompt":"hi"}`, 400},
		{"POST", "/api/sessions", `{"repo":"acme/app","prompt":"hi","unknown":1}`, 400},
		{"GET", "/api/sessions/nope", "", 404},
		{"POST", "/api/sessions/nope/messages", `{"text":"x"}`, 404},
		{"GET", "/api/sessions/" + s.ID + "/artifacts/..%2Fsecret", "", 400},
	}
	for _, c := range cases {
		if resp, b := do(t, e, c.method, c.path, c.body); resp.StatusCode != c.want {
			t.Errorf("%s %s = %d (want %d) %s", c.method, c.path, resp.StatusCode, c.want, b)
		}
	}
	// State-changing requests must be JSON (CSRF: forces a CORS preflight).
	req, _ := http.NewRequest("POST", e.Server.URL+"/api/sessions", strings.NewReader("repo=acme/app"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("form post = %d", resp.StatusCode)
	}
}

func TestEventStreamReplaysAndResumes(t *testing.T) {
	e := testenv.New(t)
	s := createSession(t, e)
	e.WaitIdle(s.ID)
	all := len(e.EventsOf(s.ID))

	read := func(lastID string) []int64 {
		req, _ := http.NewRequest("GET", e.Server.URL+"/api/sessions/"+s.ID+"/events", nil)
		if lastID != "" {
			req.Header.Set("Last-Event-ID", lastID)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var seqs []int64
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			line := sc.Text()
			if line == "event: ready" {
				return seqs
			}
			if strings.HasPrefix(line, "data: ") {
				var ev struct{ Seq int64 }
				_ = json.Unmarshal([]byte(line[6:]), &ev)
				seqs = append(seqs, ev.Seq)
			}
		}
		return seqs
	}
	if got := read(""); len(got) != all || got[0] != 1 {
		t.Fatalf("replay = %v (want %d events)", got, all)
	}
	if got := read("2"); len(got) != all-2 || got[0] != 3 {
		t.Fatalf("resume from 2 = %v", got)
	}
}

func TestEventStreamPushesLiveEvents(t *testing.T) {
	e := testenv.New(t)
	s := createSession(t, e)
	e.WaitIdle(s.ID)
	resp, err := http.Get(e.Server.URL + "/api/sessions/" + s.ID + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	lines := make(chan string, 100)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	for l := range lines {
		if l == "event: ready" {
			break
		}
	}
	_ = e.M.Send(s.ID, "live message", "u")
	timeout := time.After(3 * time.Second)
	for {
		select {
		case l := <-lines:
			if strings.Contains(l, "live message") {
				return
			}
		case <-timeout:
			t.Fatal("live event not delivered")
		}
	}
}

func TestPreviewProxiesToTheSandbox(t *testing.T) {
	e := testenv.New(t)
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("dev server, host=" + r.Host))
	}))
	defer app.Close()
	u, _ := url.Parse(app.URL)
	e.RT.Preview = u.Host
	s := createSession(t, e)
	e.WaitIdle(s.ID)

	get := func(host string) (int, string) {
		req, _ := http.NewRequest("GET", e.Server.URL+"/", nil)
		req.Host = host
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	code, body := get("p5173-" + s.ID + ".preview.remodev.test")
	if code != 200 || body != "dev server, host=localhost:5173" {
		t.Fatalf("preview = %d %q", code, body)
	}
	if code, _ := get("p5173-0000000000.preview.remodev.test"); code != 404 {
		t.Fatalf("unknown session preview = %d", code)
	}
	_ = e.M.Stop(t.Context(), s.ID)
	if code, _ := get("p5173-" + s.ID + ".preview.remodev.test"); code != 404 {
		t.Fatalf("stopped session preview = %d", code)
	}
}

func TestRequestsFromSandboxNetworksAreRejected(t *testing.T) {
	e := testenv.New(t)
	e.RT.Subnet = "127.0.0.0/8" // the test client connects from loopback
	s := createSession(t, e)
	e.WaitIdle(s.ID)
	if resp, _ := do(t, e, "GET", "/api/sessions", ""); resp.StatusCode != 403 {
		t.Fatalf("request from sandbox subnet = %d, want 403", resp.StatusCode)
	}
}
