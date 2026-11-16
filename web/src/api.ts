// Thin client for the Go API. No business logic here.
export type Status = "starting" | "running" | "idle" | "stopped" | "failed" | "deleted";

export interface Access {
  view: boolean;
  write: boolean;
  delete: boolean;
  changeVisibility: boolean;
}

export type Visibility = "team" | "private";

export interface User {
  login: string;
  name: string;
  avatarUrl: string;
}

export interface Session {
  id: string;
  visibility: Visibility;
  access: Access;
  title: string;
  repo: string;
  baseBranch: string;
  branch: string;
  agent: string;
  createdBy: string;
  status: Status;
  error?: string;
  prUrl?: string;
  turns: number;
  costUsd: number;
  createdAt: string;
  updatedAt: string;
  busy: boolean;
  previewBase: string;
}

export interface Event {
  seq: number;
  time: string;
  type: string;
  text?: string;
  data?: any;
}

export interface Artifact {
  name: string;
  size: number;
  type: string;
  modTime: string;
}

export interface Diff {
  source: "sandbox" | "github" | "none";
  diff: string;
  untracked: string[];
  truncated: boolean;
}

export interface CheckRun {
  name: string;
  status: string;
  conclusion: string;
  html_url: string;
}

export interface Checks {
  pr: { html_url: string; number: number; title: string; state: string; draft: boolean; merged_at: string | null } | null;
  runs: CheckRun[];
}

export interface Config {
  allowedRepos: string[];
  agents: { name: string; description: string }[];
  user: User;
  authEnabled: boolean;
}

async function call<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(path, {
    method,
    headers: body !== undefined ? { "Content-Type": "application/json" } : undefined,
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });
  const json = await res.json().catch(() => ({}));
  if (res.status === 401) {
    location.href = "/auth/login?next=" + encodeURIComponent(location.pathname + location.search + location.hash);
  }
  if (!res.ok) throw new Error(json.error ?? res.statusText);
  return json as T;
}

export const api = {
  config: () => call<Config>("GET", "/api/config"),
  list: () => call<Session[]>("GET", "/api/sessions"),
  get: (id: string) => call<Session>("GET", `/api/sessions/${id}`),
  create: (b: { repo: string; prompt: string; agent?: string; baseBranch?: string; visibility?: Visibility }) =>
    call<Session>("POST", "/api/sessions", b),
  send: (id: string, text: string) => call("POST", `/api/sessions/${id}/messages`, { text }),
  interrupt: (id: string) => call("POST", `/api/sessions/${id}/interrupt`, {}),
  setVisibility: (id: string, visibility: Visibility) => call<Session>("PATCH", `/api/sessions/${id}`, { visibility }),
  logout: () => call("POST", "/api/logout", {}),
  stop: (id: string) => call("POST", `/api/sessions/${id}/stop`, {}),
  remove: (id: string) => call("DELETE", `/api/sessions/${id}`, {}),
  artifacts: (id: string) => call<Artifact[]>("GET", `/api/sessions/${id}/artifacts`),
  artifactUrl: (id: string, name: string) => `/api/sessions/${id}/artifacts/${encodeURIComponent(name)}`,
  diff: (id: string) => call<Diff>("GET", `/api/sessions/${id}/diff`),
  checks: (id: string) => call<Checks>("GET", `/api/sessions/${id}/checks`),
  events: (id: string) => new EventSource(`/api/sessions/${id}/events`),
};
