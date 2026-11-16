// Package session orchestrates sandboxes and Claude Code turns.
package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/naotama2002/remodev/internal/ca"
	"github.com/naotama2002/remodev/internal/config"
	"github.com/naotama2002/remodev/internal/github"
	"github.com/naotama2002/remodev/internal/policy"
	"github.com/naotama2002/remodev/internal/store"
)

const (
	workspace = "/workspace"
	repoDir   = "/workspace/repo"
)

var (
	ErrBusy     = errors.New("session is busy")
	ErrNotFound = errors.New("session not found")
	ErrGone     = errors.New("session has been deleted")
	repoRe      = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	branchRe    = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)
)

type Manager struct {
	Cfg   *config.Config
	Store *store.Store
	RT    Runtime
	CA    *ca.CA
	GH    *github.Client
	Log   *slog.Logger

	mu      sync.Mutex
	active  map[string]*turnState // sessions with a turn in progress or queued
	subnets []*net.IPNet
	// Notify is called after every turn (used by Slack). Optional.
	Notify func(s store.Session, finalText string)
}

type turnState struct {
	cancel  context.CancelFunc
	pending []string
}

type CreateRequest struct {
	Repo       string `json:"repo"`
	Prompt     string `json:"prompt"`
	Agent      string `json:"agent,omitempty"`
	BaseBranch string `json:"baseBranch,omitempty"`
	Title      string `json:"title,omitempty"`
	CreatedBy  string `json:"-"`
	Source     string `json:"-"`
	SlackChan  string `json:"-"`
	SlackTS    string `json:"-"`
}

func newID() string {
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func randToken() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (m *Manager) Init(ctx context.Context) {
	m.active = map[string]*turnState{}
	// Recover from a crash/redeploy: turns that were running are lost, but the
	// Claude Code transcript lives in the workspace volume and can be resumed.
	for _, s := range m.Store.List() {
		switch s.Status {
		case store.StatusRunning, store.StatusStarting:
			m.Store.Update(s.ID, func(x *store.Session) { x.Status = store.StatusIdle })
			m.event(s.ID, "system", "control plane restarted; the running turn was interrupted. Send a message to continue.", nil)
		}
		if s.Status != store.StatusStopped && s.Status != store.StatusDeleted && s.Status != store.StatusFailed {
			m.refreshSubnets(ctx, s.ID)
			go m.followEgress(s.ID)
		}
	}
}

func (m *Manager) event(id, typ, text string, data any) {
	var raw json.RawMessage
	if data != nil {
		raw, _ = json.Marshal(data)
	}
	if _, err := m.Store.Append(id, store.Event{Type: typ, Text: text, Data: raw}); err != nil {
		m.Log.Error("append event", "session", id, "err", err)
	}
}

func (m *Manager) setStatus(id string, st store.Status, errMsg string) {
	s, err := m.Store.Update(id, func(s *store.Session) {
		s.Status = st
		s.Error = errMsg
		s.LastActiveAt = time.Now().UTC()
	})
	if err == nil {
		m.event(id, "status", string(st), map[string]any{"status": st, "error": errMsg, "prUrl": s.PRURL})
	}
}

func (m *Manager) Create(ctx context.Context, req CreateRequest) (store.Session, error) {
	req.Repo = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(req.Repo, "https://github.com/"), ".git"))
	if !repoRe.MatchString(req.Repo) {
		return store.Session{}, fmt.Errorf("repo must be owner/name")
	}
	if !m.Cfg.RepoAllowed(req.Repo) {
		return store.Session{}, fmt.Errorf("repo %s is not in REMODEV_ALLOWED_REPOS", req.Repo)
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return store.Session{}, fmt.Errorf("prompt is required")
	}
	if _, ok := m.Cfg.Agent(req.Agent); !ok {
		return store.Session{}, fmt.Errorf("unknown agent %q", req.Agent)
	}
	repo, err := m.GH.Repo(ctx, req.Repo)
	if err != nil {
		return store.Session{}, fmt.Errorf("cannot access repo: %w", err)
	}
	if req.BaseBranch == "" {
		req.BaseBranch = repo.DefaultBranch
	}
	if !branchRe.MatchString(req.BaseBranch) {
		return store.Session{}, fmt.Errorf("invalid base branch")
	}
	title := req.Title
	if title == "" {
		title = firstLine(req.Prompt, 80)
	}
	id := newID()
	s := &store.Session{
		ID: id, Title: title, Repo: repo.FullName, BaseBranch: req.BaseBranch,
		Branch: "remodev/" + id, Agent: orDefault(req.Agent), CreatedBy: req.CreatedBy,
		Status: store.StatusStarting, Source: req.Source, SlackChannel: req.SlackChan, SlackThread: req.SlackTS,
	}
	if err := m.Store.Create(s); err != nil {
		return store.Session{}, err
	}
	m.event(id, "user", req.Prompt, map[string]any{"by": req.CreatedBy})
	m.enqueue(id, req.Prompt)
	return *s, nil
}

func orDefault(a string) string {
	if a == "" {
		return "default"
	}
	return a
}

func firstLine(s string, n int) string {
	s = strings.TrimSpace(strings.SplitN(strings.TrimSpace(s), "\n", 2)[0])
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// Send adds a follow-up message. It is queued if a turn is in progress.
func (m *Manager) Send(id, text, by string) error {
	s, ok := m.Store.Get(id)
	if !ok {
		return ErrNotFound
	}
	if s.Status == store.StatusDeleted {
		return ErrGone
	}
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("text is required")
	}
	m.event(id, "user", text, map[string]any{"by": by})
	m.enqueue(id, text)
	return nil
}

func (m *Manager) enqueue(id, prompt string) {
	m.mu.Lock()
	if st, ok := m.active[id]; ok {
		st.pending = append(st.pending, prompt)
		m.mu.Unlock()
		m.event(id, "system", "queued: will run after the current turn", nil)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.active[id] = &turnState{cancel: cancel}
	m.mu.Unlock()
	go m.worker(ctx, id, prompt)
}

func (m *Manager) worker(ctx context.Context, id, prompt string) {
	for {
		m.runTurn(ctx, id, prompt)
		m.mu.Lock()
		st := m.active[id]
		if ctx.Err() != nil || st == nil || len(st.pending) == 0 {
			delete(m.active, id)
			m.mu.Unlock()
			return
		}
		prompt = strings.Join(st.pending, "\n\n")
		st.pending = nil
		m.mu.Unlock()
	}
}

// Interrupt stops the running turn (and drops queued messages).
func (m *Manager) Interrupt(ctx context.Context, id string) {
	m.mu.Lock()
	st := m.active[id]
	m.mu.Unlock()
	if st != nil {
		st.cancel()
	}
	// Kill claude inside the sandbox; exec contexts do not propagate signals.
	_, _ = m.RT.Exec(ctx, id, []string{"pkill", "-INT", "-f", "claude"}, "/", nil, nil)
}

// Stop removes the sandbox but keeps the workspace volume.
func (m *Manager) Stop(ctx context.Context, id string) error {
	m.Interrupt(ctx, id)
	if err := m.RT.Down(ctx, id); err != nil {
		return err
	}
	m.setStatus(id, store.StatusStopped, "")
	m.refreshSubnets(ctx, "")
	return nil
}

// Delete removes the sandbox and its workspace; the event log is kept.
func (m *Manager) Delete(ctx context.Context, id string) error {
	m.Interrupt(ctx, id)
	if err := m.RT.Destroy(ctx, id); err != nil {
		return err
	}
	m.setStatus(id, store.StatusDeleted, "")
	m.refreshSubnets(ctx, "")
	return nil
}

// ensureUp provisions the sandbox when needed and prepares the repository.
func (m *Manager) ensureUp(ctx context.Context, s store.Session) error {
	if m.RT.Alive(ctx, s.ID) {
		// Containers are up; re-run the idempotent init in case a previous attempt failed.
		return m.initWorkspace(ctx, s.ID, time.Now())
	}
	m.setStatus(s.ID, store.StatusStarting, "")
	start := time.Now()
	image, err := m.resolveImage(ctx, s.ID, s.Repo, s.BaseBranch)
	if err != nil {
		return err
	}
	agent, _ := m.Cfg.Agent(s.Agent)
	oauth := m.Cfg.AnthropicAPIKey == ""
	prof, err := policy.Render(policy.Input{
		SessionID: s.ID, Repo: s.Repo, NetworkProfile: m.Cfg.NetworkProfile,
		AllowDomains: append(append([]string{}, m.Cfg.ExtraAllowDomains...), agent.AllowDomains...),
		AllowGraphQL: m.Cfg.AllowGraphQL, AnthropicOAuth: oauth,
	})
	if err != nil {
		return err
	}
	pass := randToken()
	anthCred := m.Cfg.AnthropicAPIKey
	if oauth {
		anthCred = m.Cfg.ClaudeOAuthToken
	}
	proxyURL := "http://nono:" + pass + "@proxy:8080"
	sandboxEnv := []string{
		"HTTPS_PROXY=" + proxyURL, "HTTP_PROXY=" + proxyURL,
		"https_proxy=" + proxyURL, "http_proxy=" + proxyURL,
		"NO_PROXY=localhost,127.0.0.1,::1", "no_proxy=localhost,127.0.0.1,::1",
		"HOME=/workspace/home",
		// Phantom tokens: meaningless outside this session's proxy.
		"GH_TOKEN=ghp_" + pass,
		"REMODEV_SESSION=" + s.ID, "REMODEV_REPO=" + s.Repo,
		"REMODEV_BRANCH=" + s.Branch, "REMODEV_BASE=" + s.BaseBranch,
	}
	if oauth {
		sandboxEnv = append(sandboxEnv, "CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-"+pass)
	} else {
		sandboxEnv = append(sandboxEnv, "ANTHROPIC_API_KEY="+pass)
	}
	spec := Spec{
		ID: s.ID, ProxyPass: pass, PolicyJSON: prof, CACert: m.CA.CertPEM, CAKey: m.CA.KeyPEM,
		ProxyEnv: []string{
			policy.EnvGitHubToken + "=" + m.Cfg.GitHubToken,
			policy.EnvGitHubBasic + "=x-access-token:" + m.Cfg.GitHubToken,
			policy.EnvAnthropic + "=" + anthCred,
		},
		SandboxEnv: sandboxEnv, Memory: m.Cfg.SandboxMemory, NanoCPUs: int64(m.Cfg.SandboxCPUs * 1e9),
		SandboxImage: image, ProxyImage: m.Cfg.ProxyImage,
	}
	if err := m.RT.Up(ctx, spec); err != nil {
		return err
	}
	m.refreshSubnets(ctx, s.ID)
	go m.followEgress(s.ID)
	return m.initWorkspace(ctx, s.ID, start)
}

func (m *Manager) initWorkspace(ctx context.Context, id string, start time.Time) error {
	var out []string
	code, err := m.RT.Exec(ctx, id, []string{"remodev-init"}, "/", func(l string) {
		out = append(out, l)
		m.event(id, "system", l, nil)
	}, func(l string) { out = append(out, l) })
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("remodev-init failed (exit %d): %s", code, tail(out, 8))
	}
	m.event(id, "system", fmt.Sprintf("sandbox ready in %.1fs", time.Since(start).Seconds()), nil)
	return nil
}

func tail(lines []string, n int) string {
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func (m *Manager) runTurn(ctx context.Context, id, prompt string) {
	s, ok := m.Store.Get(id)
	if !ok || s.Status == store.StatusDeleted {
		return
	}
	if err := m.ensureUp(ctx, s); err != nil {
		m.Log.Error("sandbox up", "session", id, "err", err)
		m.event(id, "error", err.Error(), nil)
		m.setStatus(id, store.StatusFailed, err.Error())
		return
	}
	m.setStatus(id, store.StatusRunning, "")
	agent, _ := m.Cfg.Agent(s.Agent)
	args := []string{"claude", "-p", prompt, "--output-format", "stream-json", "--verbose",
		"--dangerously-skip-permissions", "--append-system-prompt", systemPrompt(s, agent)}
	if agent.Model != "" {
		args = append(args, "--model", agent.Model)
	}
	if s.ClaudeSessionID != "" {
		args = append(args, "--resume", s.ClaudeSessionID)
	}
	var final string
	var stderr []string
	code, err := m.RT.Exec(ctx, id, args, repoDir, func(line string) {
		if f := m.handleStreamLine(id, line); f != "" {
			final = f
		}
	}, func(l string) { stderr = append(stderr, l) })
	switch {
	case ctx.Err() != nil:
		m.event(id, "system", "interrupted", nil)
	case err != nil:
		m.event(id, "error", err.Error(), nil)
	case code != 0:
		m.event(id, "error", fmt.Sprintf("claude exited with %d: %s", code, tail(stderr, 10)), nil)
	}
	m.updatePR(context.WithoutCancel(ctx), id)
	m.setStatus(id, store.StatusIdle, "")
	if m.Notify != nil {
		if cur, ok := m.Store.Get(id); ok {
			m.Notify(cur, final)
		}
	}
}

func (m *Manager) updatePR(ctx context.Context, id string) {
	s, _ := m.Store.Get(id)
	pr, err := m.GH.FindPR(ctx, s.Repo, s.Branch)
	if err != nil || pr == nil || pr.HTMLURL == s.PRURL {
		return
	}
	m.Store.Update(id, func(x *store.Session) { x.PRURL = pr.HTMLURL })
	m.event(id, "pr", pr.HTMLURL, pr)
}

// ---- Claude Code stream-json -------------------------------------------------

type streamMsg struct {
	Type      string  `json:"type"`
	Subtype   string  `json:"subtype"`
	SessionID string  `json:"session_id"`
	Result    string  `json:"result"`
	IsError   bool    `json:"is_error"`
	CostUSD   float64 `json:"total_cost_usd"`
	NumTurns  int     `json:"num_turns"`
	Message   struct {
		Content []struct {
			Type      string          `json:"type"`
			Text      string          `json:"text"`
			Name      string          `json:"name"`
			ID        string          `json:"id"`
			Input     json.RawMessage `json:"input"`
			ToolUseID string          `json:"tool_use_id"`
			Content   json.RawMessage `json:"content"`
			IsError   bool            `json:"is_error"`
		} `json:"content"`
	} `json:"message"`
}

// handleStreamLine converts one stream-json line into events. Returns the
// final result text when the line is the result message.
func (m *Manager) handleStreamLine(id, line string) string {
	var msg streamMsg
	if err := json.Unmarshal([]byte(line), &msg); err != nil {
		if strings.TrimSpace(line) != "" {
			m.event(id, "system", line, nil)
		}
		return ""
	}
	switch msg.Type {
	case "system":
		if msg.Subtype == "init" && msg.SessionID != "" {
			m.Store.Update(id, func(s *store.Session) { s.ClaudeSessionID = msg.SessionID })
		}
	case "assistant":
		for _, c := range msg.Message.Content {
			switch c.Type {
			case "text":
				if strings.TrimSpace(c.Text) != "" {
					m.event(id, "assistant", c.Text, nil)
				}
			case "tool_use":
				m.event(id, "tool_use", c.Name, map[string]any{"id": c.ID, "name": c.Name, "input": clip(c.Input, 4000)})
			}
		}
	case "user":
		for _, c := range msg.Message.Content {
			if c.Type == "tool_result" {
				m.event(id, "tool_result", toolResultText(c.Content, 4000), map[string]any{"toolUseId": c.ToolUseID, "isError": c.IsError})
			}
		}
	case "result":
		m.Store.Update(id, func(s *store.Session) {
			s.Turns++
			s.CostUSD += msg.CostUSD
			if msg.SessionID != "" {
				s.ClaudeSessionID = msg.SessionID
			}
		})
		m.event(id, "result", msg.Result, map[string]any{"isError": msg.IsError, "costUsd": msg.CostUSD, "numTurns": msg.NumTurns, "subtype": msg.Subtype})
		return msg.Result
	}
	return ""
}

func clip(raw json.RawMessage, n int) json.RawMessage {
	if len(raw) <= n {
		return raw
	}
	b, _ := json.Marshal(string(raw[:n]) + "…")
	return b
}

func toolResultText(raw json.RawMessage, n int) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(raw, &parts) == nil {
			var b strings.Builder
			for _, p := range parts {
				if p.Type == "text" {
					b.WriteString(p.Text)
				} else {
					b.WriteString("[" + p.Type + "]")
				}
			}
			s = b.String()
		} else {
			s = string(raw)
		}
	}
	if r := []rune(s); len(r) > n {
		s = string(r[:n]) + "…"
	}
	return s
}

func systemPrompt(s store.Session, a config.Agent) string {
	p := fmt.Sprintf(`You are running inside remodev, an isolated cloud sandbox, on behalf of a developer.
- The repository %[1]s is checked out at %[2]s on branch %[3]s (based on %[4]s).
- Network access goes through an egress proxy with an allow list. Real credentials are injected by the proxy; the tokens in your environment are placeholders, which is expected.
- You may push only to the branch %[3]s of %[1]s. Commit your work with clear messages and push with: git push origin HEAD:%[3]s
- To open or update the pull request run: remodev-pr "<title>" "<body markdown>"  (uses the GitHub REST API; do not use gh pr create).
- If a request is denied by the proxy (HTTP 403 / "not in the allowlist"), do not try to bypass it; explain what access you need.
- To let the user preview a web server, bind it to 0.0.0.0 and run it in the background (e.g. remodev-bg npm run dev -- --host 0.0.0.0), then tell the user the port.
- Finish each turn with a short summary of what you did and what is left.`, s.Repo, repoDir, s.Branch, s.BaseBranch)
	if a.AppendSystemPrompt != "" {
		p += "\n\n" + a.AppendSystemPrompt
	}
	return p
}

// ---- egress log -----------------------------------------------------------------

var denyRe = regexp.MustCompile(`(?i)\b(denied|deny)\b`)

func (m *Manager) followEgress(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 24*time.Hour)
	defer cancel()
	_ = m.RT.FollowEgress(ctx, id, time.Now(), func(line string) {
		if !denyRe.MatchString(line) {
			return
		}
		m.event(id, "egress", compactEgress(line), nil)
	})
}

var kvRe = regexp.MustCompile(`(host|target|method|path|reason|status)=("[^"]*"|\S+)`)

func compactEgress(line string) string {
	kv := kvRe.FindAllStringSubmatch(line, -1)
	if len(kv) == 0 {
		return line
	}
	parts := make([]string, 0, len(kv))
	for _, m := range kv {
		parts = append(parts, m[1]+"="+strings.Trim(m[2], `"`))
	}
	return "denied: " + strings.Join(parts, " ")
}

// ---- lifecycle --------------------------------------------------------------------

// Reap stops idle sandboxes and deletes old workspaces. Call periodically.
func (m *Manager) Reap(ctx context.Context) {
	now := time.Now()
	for _, s := range m.Store.List() {
		m.mu.Lock()
		_, busy := m.active[s.ID]
		m.mu.Unlock()
		if busy {
			continue
		}
		idle := now.Sub(s.LastActiveAt)
		switch {
		case (s.Status == store.StatusIdle || s.Status == store.StatusFailed) && idle > m.Cfg.IdleStop:
			m.Log.Info("stopping idle sandbox", "session", s.ID)
			if err := m.Stop(ctx, s.ID); err != nil {
				m.Log.Error("stop", "session", s.ID, "err", err)
			}
		case s.Status == store.StatusStopped && idle > m.Cfg.Retain:
			m.Log.Info("deleting old workspace", "session", s.ID)
			if err := m.Delete(ctx, s.ID); err != nil {
				m.Log.Error("delete", "session", s.ID, "err", err)
			}
		}
	}
}

// ---- sandbox source-IP filter ------------------------------------------------------

func (m *Manager) refreshSubnets(ctx context.Context, _ string) {
	var nets []*net.IPNet
	for _, s := range m.Store.List() {
		if s.Status == store.StatusStopped || s.Status == store.StatusDeleted {
			continue
		}
		cidrs, err := m.RT.Subnets(ctx, s.ID)
		if err != nil {
			continue
		}
		for _, c := range cidrs {
			if _, n, err := net.ParseCIDR(c); err == nil {
				nets = append(nets, n)
			}
		}
	}
	m.mu.Lock()
	m.subnets = nets
	m.mu.Unlock()
}

// FromSandbox reports whether ip belongs to a session network. The control
// plane joins those networks to reach previews, so it must refuse API calls
// that originate from a sandbox.
func (m *Manager) FromSandbox(ip net.IP) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, n := range m.subnets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func (m *Manager) Busy(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.active[id]
	return ok
}
