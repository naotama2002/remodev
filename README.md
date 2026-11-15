# remodev

Self-hosted background coding agent (inspired by LayerX "haro", Ubie "Remoco/unaba", Flatt "Sunaba").
Give it a repository and a task; it starts an isolated sandbox, runs Claude Code in YOLO mode, and opens a draft PR.

## Architecture

```
browser ──► control (Go, single binary, embeds UI)
              │  docker API
              ▼  per session:
     ┌───────────────── internal network (no route out) ─────────────────┐
     │  sandbox (Claude Code, git, gh)  ──HTTPS_PROXY──►  nono proxy ─────┼──► internet
     │  phantom tokens only                               real creds,     │    (allow list)
     │  /workspace volume (kept after stop)               L7 rules        │
     └────────────────────────────────────────────────────────────────────┘
```

- **No secrets in the sandbox.** Env vars hold placeholder tokens; the per-session [nono](https://github.com/nolabs-ai/nono) proxy injects real credentials only for allowed endpoints.
- **Default-deny egress.** The sandbox sits on an `internal` Docker network; the proxy is its only exit. Hosts come from the nono `developer` profile plus `REMODEV_ALLOW_DOMAINS`.
- **GitHub writes are limited to the session repository** (git push, PRs, issues, comments). Deletes, secrets, hooks, keys, rulesets and branch protection are denied. Protect `main` with a GitHub ruleset; nono does not inspect git packs.
- **Lifecycle.** Idle sandboxes stop after `REMODEV_IDLE_STOP` (the workspace volume is kept, and the next message resumes the Claude Code session). Workspaces are deleted after `REMODEV_RETAIN`; the event log is kept.
- **Thin UI.** Go holds all logic. `web/` is React + Vite with 5 direct dependencies, built at image build time and embedded into the binary. No Node at runtime.

## Quick start (local)

```sh
cp .env.example .env    # set REMODEV_GITHUB_TOKEN and a Claude credential
make images             # sandbox + nono proxy images
docker compose up --build -d
open http://localhost:8080
```

Claude credential: `REMODEV_ANTHROPIC_API_KEY`, or a subscription token from `claude setup-token` in `REMODEV_CLAUDE_OAUTH_TOKEN`.

Previews: run a dev server bound to `0.0.0.0` in the sandbox and open `http://p<port>-<session>.preview.localhost:8080`.

## Layout

```
cmd/remodev        entrypoint
internal/docker    zero-dependency Docker Engine API client
internal/session   session manager, runtime (Docker), Claude Code stream-json handling
internal/policy    per-session nono profile (egress + credential routes)
internal/store     file-based session/event store (JSONL)
internal/server    REST + SSE API, preview reverse proxy, UI serving
images/sandbox     sandbox image (Claude Code native binary, gh, helpers)
images/nono        egress proxy image (pinned, checksum-verified)
web/               UI (React + Vite)
```

## Dependency policy

- Go: standard library only.
- Downloaded binaries (Claude Code, gh, nono) and base images are pinned by version and sha256.
- `web/`: exact versions, `ignore-scripts`, `minimumReleaseAge` 7 days, frozen lockfile.

## Roadmap

- [ ] Authentication (GitHub OAuth / ALB OIDC) for team use
- [ ] Slack (start sessions from threads, post results)
- [ ] AWS deployment (Terraform: EC2 spot host for Docker runtime, ALB + OIDC)
- [ ] GitHub App tokens instead of a PAT
- [ ] Custom agents (system prompt / MCP / allow list presets) from a file
- [ ] Stronger isolation (gVisor `runsc` / Kata / Firecracker)
- [ ] Outer loop: daily analysis of sessions to improve prompts and skills
