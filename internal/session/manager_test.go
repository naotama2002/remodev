package session_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/naotama2002/remodev/internal/fake"
	"github.com/naotama2002/remodev/internal/github"
	"github.com/naotama2002/remodev/internal/session"
	"github.com/naotama2002/remodev/internal/store"
	"github.com/naotama2002/remodev/internal/testenv"
)

func create(t *testing.T, e *testenv.Env, prompt string) store.Session {
	t.Helper()
	s, err := e.M.Create(context.Background(), session.CreateRequest{Repo: "acme/app", Prompt: prompt, CreatedBy: "alice", Source: "test"})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestCreateRunsATurnAndRecordsTheConversation(t *testing.T) {
	e := testenv.New(t)
	s := create(t, e, "fix the bug")
	if s.Branch != "remodev/"+s.ID || s.BaseBranch != "main" || s.CreatedBy != "alice" {
		t.Fatalf("unexpected session: %+v", s)
	}
	got := e.WaitIdle(s.ID)
	if got.Status != store.StatusIdle || got.Turns != 1 || got.ClaudeSessionID == "" {
		t.Fatalf("after turn: %+v", got)
	}
	var texts []string
	for _, ev := range e.EventsOf(s.ID, "user", "assistant", "result") {
		texts = append(texts, ev.Type+":"+ev.Text)
	}
	want := "user:fix the bug|assistant:echo: fix the bug|result:echo: fix the bug"
	if strings.Join(texts, "|") != want {
		t.Fatalf("conversation = %q", strings.Join(texts, "|"))
	}
}

func TestCreateValidatesInput(t *testing.T) {
	e := testenv.New(t)
	for name, req := range map[string]session.CreateRequest{
		"repo not allowed": {Repo: "evil/app", Prompt: "x"},
		"bad repo":         {Repo: "acme", Prompt: "x"},
		"empty prompt":     {Repo: "acme/app", Prompt: "  "},
		"unknown agent":    {Repo: "acme/app", Prompt: "x", Agent: "nope"},
		"repo not on gh":   {Repo: "acme/missing", Prompt: "x"},
	} {
		if _, err := e.M.Create(context.Background(), req); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	if n := len(e.Store.List()); n != 0 {
		t.Fatalf("invalid requests created %d sessions", n)
	}
}

func TestSandboxGetsOnlyPlaceholderCredentials(t *testing.T) {
	e := testenv.New(t)
	s := create(t, e, "hi")
	e.WaitIdle(s.ID)
	spec, ok := e.RT.Spec(s.ID)
	if !ok {
		t.Fatal("sandbox not up")
	}
	for _, kv := range spec.SandboxEnv {
		if strings.Contains(kv, e.Cfg.GitHubToken) || strings.Contains(kv, e.Cfg.ClaudeOAuthToken) {
			t.Fatalf("real credential leaked into sandbox env: %s", strings.SplitN(kv, "=", 2)[0])
		}
	}
	proxy := strings.Join(spec.ProxyEnv, "\n")
	if !strings.Contains(proxy, e.Cfg.GitHubToken) || !strings.Contains(proxy, e.Cfg.ClaudeOAuthToken) {
		t.Fatal("proxy must hold the real credentials")
	}
}

func TestFollowUpResumesTheClaudeSession(t *testing.T) {
	e := testenv.New(t)
	s := create(t, e, "first")
	first := e.WaitIdle(s.ID)
	if err := e.M.Send(s.ID, "second", "bob"); err != nil {
		t.Fatal(err)
	}
	e.WaitIdle(s.ID)
	turns := e.RT.Turns()
	if len(turns) != 2 || turns[0].Resume != "" || turns[1].Resume != first.ClaudeSessionID {
		t.Fatalf("turns = %+v", turns)
	}
}

func TestMessagesSentDuringATurnAreQueuedAndMerged(t *testing.T) {
	e := testenv.New(t)
	release := make(chan struct{})
	var once sync.Once
	e.RT.Claude = func(ctx context.Context, rt *fake.Runtime, tn fake.Turn, emit func(string)) int {
		once.Do(func() { <-release })
		return fake.EchoClaude(ctx, rt, tn, emit)
	}
	s := create(t, e, "first")
	waitFor(t, func() bool { return len(e.RT.Turns()) == 1 })
	_ = e.M.Send(s.ID, "a", "u")
	_ = e.M.Send(s.ID, "b", "u")
	close(release)
	e.WaitIdle(s.ID)
	turns := e.RT.Turns()
	if len(turns) != 2 || turns[1].Prompt != "a\n\nb" {
		t.Fatalf("turns = %+v", turns)
	}
}

func TestInterruptStopsTheTurn(t *testing.T) {
	e := testenv.New(t)
	e.RT.Claude = func(ctx context.Context, _ *fake.Runtime, _ fake.Turn, _ func(string)) int {
		<-ctx.Done()
		return 130
	}
	s := create(t, e, "long task")
	waitFor(t, func() bool { return len(e.RT.Turns()) == 1 })
	e.M.Interrupt(context.Background(), s.ID)
	got := e.WaitIdle(s.ID)
	if got.Status != store.StatusIdle {
		t.Fatalf("status = %s", got.Status)
	}
	if len(e.EventsOf(s.ID, "system")) == 0 || !hasText(e.EventsOf(s.ID, "system"), "interrupted") {
		t.Fatal("no interrupted event")
	}
}

func TestStopKeepsWorkspaceAndNextMessageRestartsSandbox(t *testing.T) {
	e := testenv.New(t)
	s := create(t, e, "one")
	e.WaitIdle(s.ID)
	if err := e.M.Stop(context.Background(), s.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.Store.Get(s.ID); got.Status != store.StatusStopped || e.RT.Alive(context.Background(), s.ID) {
		t.Fatalf("after stop: %s alive=%v", got.Status, e.RT.Alive(context.Background(), s.ID))
	}
	_ = e.M.Send(s.ID, "two", "u")
	got := e.WaitIdle(s.ID)
	if got.Status != store.StatusIdle || got.Turns != 2 || !e.RT.Alive(context.Background(), s.ID) {
		t.Fatalf("after restart: %+v", got)
	}
}

func TestDeleteRemovesWorkspaceAndRejectsMessages(t *testing.T) {
	e := testenv.New(t)
	s := create(t, e, "one")
	e.WaitIdle(s.ID)
	if err := e.M.Delete(context.Background(), s.ID); err != nil {
		t.Fatal(err)
	}
	if !e.RT.Destroyed(s.ID) {
		t.Fatal("workspace not destroyed")
	}
	if err := e.M.Send(s.ID, "x", "u"); err != session.ErrGone {
		t.Fatalf("send after delete: %v", err)
	}
}

func TestPRAndArtifactsAreCollectedAfterTheTurn(t *testing.T) {
	e := testenv.New(t)
	e.RT.Claude = fake.DemoClaude(e.GH)
	s := create(t, e, "add a feature")
	got := e.WaitIdle(s.ID)
	if got.PRURL == "" || len(e.EventsOf(s.ID, "pr")) != 1 {
		t.Fatalf("pr not recorded: %+v", got)
	}
	arts, err := e.M.Artifacts(s.ID)
	if err != nil || len(arts) != 1 || arts[0].Name != "demo-screenshot.png" || arts[0].Type != "image/png" {
		t.Fatalf("artifacts = %+v, %v", arts, err)
	}
	if len(e.EventsOf(s.ID, "artifact")) != 1 {
		t.Fatal("artifact event missing")
	}
	// Artifacts outlive the workspace.
	_ = e.M.Delete(context.Background(), s.ID)
	if arts, _ := e.M.Artifacts(s.ID); len(arts) != 1 {
		t.Fatal("artifacts removed with workspace")
	}
}

func TestDiffComesFromSandboxThenGitHub(t *testing.T) {
	e := testenv.New(t)
	s := create(t, e, "x")
	e.WaitIdle(s.ID)
	e.RT.SetDiff(s.ID, "diff --git a/x b/x\n+local\n")
	d, err := e.M.Diff(context.Background(), s.ID)
	if err != nil || d.Source != "sandbox" || !strings.Contains(d.Diff, "+local") {
		t.Fatalf("sandbox diff = %+v, %v", d, err)
	}
	_ = e.M.Stop(context.Background(), s.ID)
	d, _ = e.M.Diff(context.Background(), s.ID)
	if d.Source != "none" {
		t.Fatalf("before push: %+v", d)
	}
	e.GH.SetDiff(s.Branch, "diff --git a/x b/x\n+pushed\n")
	d, _ = e.M.Diff(context.Background(), s.ID)
	if d.Source != "github" || !strings.Contains(d.Diff, "+pushed") {
		t.Fatalf("github diff = %+v", d)
	}
}

func TestChecks(t *testing.T) {
	e := testenv.New(t)
	s := create(t, e, "x")
	e.WaitIdle(s.ID)
	c, err := e.M.Checks(context.Background(), s.ID)
	if err != nil || c.PR != nil {
		t.Fatalf("no PR yet: %+v %v", c, err)
	}
	e.GH.SetPR(s.Branch, &github.PR{Number: 7, HTMLURL: "u"})
	e.GH.SetCheckRuns([]github.CheckRun{{Name: "test", Status: "completed", Conclusion: "success"}})
	c, _ = e.M.Checks(context.Background(), s.ID)
	if c.PR == nil || c.PR.Number != 7 || len(c.Runs) != 1 {
		t.Fatalf("checks = %+v", c)
	}
}

func TestReapStopsIdleAndDeletesOldSessions(t *testing.T) {
	e := testenv.New(t)
	s := create(t, e, "x")
	e.WaitIdle(s.ID)
	past := func(d time.Duration) {
		_, _ = e.Store.Update(s.ID, func(x *store.Session) { x.LastActiveAt = time.Now().Add(-d) })
	}
	past(e.Cfg.IdleStop + time.Minute)
	e.M.Reap(context.Background())
	if got, _ := e.Store.Get(s.ID); got.Status != store.StatusStopped {
		t.Fatalf("idle session not stopped: %s", got.Status)
	}
	past(e.Cfg.Retain + time.Minute)
	e.M.Reap(context.Background())
	if got, _ := e.Store.Get(s.ID); got.Status != store.StatusDeleted {
		t.Fatalf("old session not deleted: %s", got.Status)
	}
}

func TestFailedProvisioningMarksSessionFailed(t *testing.T) {
	e := testenv.New(t)
	e.RT.FailUp = "no capacity"
	s := create(t, e, "x")
	got := e.WaitIdle(s.ID)
	if got.Status != store.StatusFailed || !strings.Contains(got.Error, "no capacity") {
		t.Fatalf("got %+v", got)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met")
}

func hasText(evs []store.Event, s string) bool {
	for _, ev := range evs {
		if strings.Contains(ev.Text, s) {
			return true
		}
	}
	return false
}
