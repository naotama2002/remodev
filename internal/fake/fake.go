// Package fake provides in-memory implementations of the session manager's
// dependencies (sandbox runtime, GitHub). They are used by tests and by the
// "fake" runtime mode (REMODEV_RUNTIME=fake), which runs the whole control
// plane and UI without Docker, GitHub or Claude.
package fake

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/naotama2002/remodev/internal/docker"
	"github.com/naotama2002/remodev/internal/github"
	"github.com/naotama2002/remodev/internal/session"
)

// Turn is what a fake Claude Code invocation sees.
type Turn struct {
	Session string // remodev session id
	Prompt  string
	Resume  string // --resume value ("" on the first turn)
	Model   string
}

// ClaudeFunc emulates one `claude -p` run. It writes stream-json lines with
// emit and returns the exit code. It should return when ctx is cancelled.
type ClaudeFunc func(ctx context.Context, rt *Runtime, t Turn, emit func(line string)) int

// Runtime is an in-memory session.Runtime.
type Runtime struct {
	Claude ClaudeFunc
	// Preview maps any preview request to this host:port (e.g. an httptest server).
	Preview string
	// Subnet is returned by Subnets for running sessions.
	Subnet string
	// EgressLines are emitted by FollowEgress for every session.
	EgressLines []string
	// FailUp makes Up fail with this message.
	FailUp string

	mu        sync.Mutex
	up        map[string]session.Spec
	files     map[string]map[string][]byte // session -> artifact name -> data
	diff      map[string]string
	turns     []Turn
	destroyed map[string]bool
}

func NewRuntime() *Runtime {
	return &Runtime{
		Claude: EchoClaude, up: map[string]session.Spec{}, files: map[string]map[string][]byte{},
		diff: map[string]string{}, destroyed: map[string]bool{},
	}
}

var _ session.Runtime = (*Runtime)(nil)

func (r *Runtime) Up(_ context.Context, s session.Spec) error {
	if r.FailUp != "" {
		return fmt.Errorf("fake: %s", r.FailUp)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.up[s.ID] = s
	return nil
}

func (r *Runtime) Down(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.up, id)
	return nil
}

func (r *Runtime) Destroy(ctx context.Context, id string) error {
	_ = r.Down(ctx, id)
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.files, id)
	r.destroyed[id] = true
	return nil
}

func (r *Runtime) Alive(_ context.Context, id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.up[id]
	return ok
}

func (r *Runtime) Exec(ctx context.Context, id string, cmd []string, _ string, onStdout, onStderr func(string)) (int, error) {
	out := func(s string) {
		if onStdout != nil {
			onStdout(s)
		}
	}
	if !r.Alive(ctx, id) {
		return -1, fmt.Errorf("fake: sandbox %s is not running", id)
	}
	switch {
	case cmd[0] == "remodev-init":
		out("fake workspace ready")
		return 0, nil
	case cmd[0] == "claude":
		t := Turn{Session: id}
		for i := 1; i < len(cmd); i++ {
			switch cmd[i] {
			case "-p":
				t.Prompt = cmd[i+1]
			case "--resume":
				t.Resume = cmd[i+1]
			case "--model":
				t.Model = cmd[i+1]
			}
		}
		r.mu.Lock()
		r.turns = append(r.turns, t)
		fn := r.Claude
		r.mu.Unlock()
		return fn(ctx, r, t, out), nil
	case len(cmd) > 3 && cmd[0] == "git" && slices.Contains(cmd, "diff"):
		r.mu.Lock()
		d := r.diff[id]
		r.mu.Unlock()
		for _, l := range strings.Split(strings.TrimSuffix(d, "\n"), "\n") {
			if d != "" {
				out(l)
			}
		}
		return 0, nil
	default: // git ls-files, pkill, ...
		return 0, nil
	}
}

func (r *Runtime) Spawn(context.Context, string, []string, string) error { return nil }

func (r *Runtime) Subnets(_ context.Context, id string) ([]string, error) {
	if r.Subnet == "" {
		return nil, nil
	}
	return []string{r.Subnet}, nil
}

func (r *Runtime) Archive(_ context.Context, id, _ string) (io.ReadCloser, error) {
	r.mu.Lock()
	files := r.files[id]
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	_ = tw.WriteHeader(&tar.Header{Name: "artifacts/", Typeflag: tar.TypeDir, Mode: 0o755})
	for name, data := range files {
		_ = tw.WriteHeader(&tar.Header{Name: "artifacts/" + name, Mode: 0o644, Size: int64(len(data)), ModTime: time.Now()})
		_, _ = tw.Write(data)
	}
	r.mu.Unlock()
	_ = tw.Close()
	if len(files) == 0 {
		return nil, docker.ErrNotFound
	}
	return io.NopCloser(&buf), nil
}

func (r *Runtime) FollowEgress(ctx context.Context, _ string, _ time.Time, onLine func(string)) error {
	for _, l := range r.EgressLines {
		onLine(l)
	}
	<-ctx.Done()
	return nil
}

func (r *Runtime) PreviewTarget(string, int) string { return r.Preview }

// ---- helpers for ClaudeFuncs and tests ----------------------------------------

// WriteArtifact places a file in the sandbox's /workspace/artifacts.
func (r *Runtime) WriteArtifact(id, name string, data []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.files[id] == nil {
		r.files[id] = map[string][]byte{}
	}
	r.files[id][name] = data
}

// SetDiff sets what `git diff` prints in the sandbox.
func (r *Runtime) SetDiff(id, diff string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.diff[id] = diff
}

// Turns returns every claude invocation so far.
func (r *Runtime) Turns() []Turn {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.turns)
}

// Spec returns the spec the sandbox was started with.
func (r *Runtime) Spec(id string) (session.Spec, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.up[id]
	return s, ok
}

func (r *Runtime) Destroyed(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.destroyed[id]
}

// Stream builds Claude Code stream-json lines.
type Stream struct{ emit func(string) }

func NewStream(emit func(string)) Stream { return Stream{emit} }

func (s Stream) line(v any) { b, _ := json.Marshal(v); s.emit(string(b)) }

func (s Stream) Init(sessionID string) {
	s.line(map[string]any{"type": "system", "subtype": "init", "session_id": sessionID})
}

func (s Stream) Text(text string) {
	s.line(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}})
}

func (s Stream) Tool(id, name string, input any, result string) {
	s.line(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "id": id, "name": name, "input": input}}}})
	s.line(map[string]any{"type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": id, "content": result}}}})
}

func (s Stream) Result(sessionID, text string, cost float64) {
	s.line(map[string]any{"type": "result", "subtype": "success", "session_id": sessionID, "result": text, "total_cost_usd": cost, "num_turns": 1})
}

// EchoClaude answers every prompt with "echo: <prompt>".
func EchoClaude(_ context.Context, _ *Runtime, t Turn, emit func(string)) int {
	s := NewStream(emit)
	cid := t.Resume
	if cid == "" {
		cid = "claude-" + t.Session
	}
	s.Init(cid)
	s.Text("echo: " + t.Prompt)
	s.Result(cid, "echo: "+t.Prompt, 0.01)
	return 0
}

// TinyPNG is a valid 1x1 PNG.
var TinyPNG, _ = base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==")

// DemoClaude pretends to implement the prompt: it runs a tool, saves a
// screenshot, "pushes" a diff and opens a PR on the fake GitHub.
func DemoClaude(gh *GitHub) ClaudeFunc {
	return func(ctx context.Context, rt *Runtime, t Turn, emit func(string)) int {
		s := NewStream(emit)
		cid := t.Resume
		if cid == "" {
			cid = "claude-" + t.Session
		}
		s.Init(cid)
		s.Text("了解しました。リポジトリを確認します。")
		select {
		case <-ctx.Done():
			return 130
		case <-time.After(300 * time.Millisecond):
		}
		s.Tool("t1", "Bash", map[string]string{"command": "ls"}, "README.md\ncmd\ninternal")
		rt.WriteArtifact(t.Session, "demo-screenshot.png", TinyPNG)
		rt.SetDiff(t.Session, "diff --git a/README.md b/README.md\n--- a/README.md\n+++ b/README.md\n@@ -1 +1,2 @@\n # demo\n+"+t.Prompt+"\n")
		gh.SetPR("remodev/"+t.Session, &github.PR{HTMLURL: "https://github.example/demo/pull/1", Number: 1, State: "open", Draft: true, Title: t.Prompt})
		s.Result(cid, "完了しました: "+t.Prompt, 0.02)
		return 0
	}
}

// ---- GitHub -----------------------------------------------------------------

type GitHub struct {
	// AnyRepo makes Repo succeed for every name (fake runtime mode).
	AnyRepo bool

	mu    sync.Mutex
	repos map[string]*github.Repo
	prs   map[string]*github.PR // branch -> PR
	runs  []github.CheckRun
	diffs map[string]string // branch -> diff
	dirs  map[string][]github.File
}

func NewGitHub(repos ...string) *GitHub {
	g := &GitHub{repos: map[string]*github.Repo{}, prs: map[string]*github.PR{}, diffs: map[string]string{}, dirs: map[string][]github.File{}}
	for _, r := range repos {
		g.repos[strings.ToLower(r)] = &github.Repo{FullName: r, DefaultBranch: "main"}
	}
	return g
}

var _ session.GitHub = (*GitHub)(nil)

func (g *GitHub) SetPR(branch string, pr *github.PR) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.prs[branch] = pr
}

func (g *GitHub) SetCheckRuns(runs []github.CheckRun) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.runs = runs
}

func (g *GitHub) SetDiff(branch, diff string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.diffs[branch] = diff
}

func (g *GitHub) Repo(_ context.Context, name string) (*github.Repo, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	r, ok := g.repos[strings.ToLower(name)]
	if !ok && g.AnyRepo {
		return &github.Repo{FullName: name, DefaultBranch: "main"}, nil
	}
	if !ok {
		return nil, fmt.Errorf("repo %s: %w", name, github.ErrNotFound)
	}
	cp := *r
	return &cp, nil
}

func (g *GitHub) FindPR(_ context.Context, _, branch string) (*github.PR, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.prs[branch], nil
}

func (g *GitHub) CheckRuns(context.Context, string, string) ([]github.CheckRun, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.runs), nil
}

func (g *GitHub) CompareDiff(_ context.Context, _, _, head string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	d, ok := g.diffs[head]
	if !ok {
		return "", github.ErrNotFound
	}
	return d, nil
}

func (g *GitHub) Dir(_ context.Context, repo, dir, _ string) ([]github.File, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	f, ok := g.dirs[repo+"/"+dir]
	if !ok {
		return nil, github.ErrNotFound
	}
	return f, nil
}
