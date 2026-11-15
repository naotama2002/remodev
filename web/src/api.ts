// Thin client for the Go API. No business logic here.
export type Status = "starting" | "running" | "idle" | "stopped" | "failed" | "deleted";

export interface Session {
  id: string;
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

export interface Config {
  allowedRepos: string[];
  agents: { name: string; description: string }[];
  user: string;
}

async function call<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(path, {
    method,
    headers: body !== undefined ? { "Content-Type": "application/json" } : undefined,
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });
  const json = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(json.error ?? res.statusText);
  return json as T;
}

export const api = {
  config: () => call<Config>("GET", "/api/config"),
  list: () => call<Session[]>("GET", "/api/sessions"),
  get: (id: string) => call<Session>("GET", `/api/sessions/${id}`),
  create: (b: { repo: string; prompt: string; agent?: string; baseBranch?: string }) =>
    call<Session>("POST", "/api/sessions", b),
  send: (id: string, text: string) => call("POST", `/api/sessions/${id}/messages`, { text }),
  interrupt: (id: string) => call("POST", `/api/sessions/${id}/interrupt`, {}),
  stop: (id: string) => call("POST", `/api/sessions/${id}/stop`, {}),
  remove: (id: string) => call("DELETE", `/api/sessions/${id}`, {}),
  events: (id: string) => new EventSource(`/api/sessions/${id}/events`),
};
