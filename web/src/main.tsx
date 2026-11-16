import { StrictMode, useCallback, useEffect, useRef, useState } from "react";
import { createRoot } from "react-dom/client";
import { api, type Config, type Event, type Session } from "./api";
import { appendEvent, sessionRoute } from "./lib";
import { SidePanel } from "./panel";
import "./style.css";

function useHash(): [string, (h: string) => void] {
  const [h, setH] = useState(location.hash.slice(1));
  useEffect(() => {
    const f = () => setH(location.hash.slice(1));
    addEventListener("hashchange", f);
    return () => removeEventListener("hashchange", f);
  }, []);
  return [h, (v) => (location.hash = v)];
}

function App() {
  const [cfg, setCfg] = useState<Config>();
  const [sessions, setSessions] = useState<Session[]>([]);
  const [route, go] = useHash();
  const refresh = useCallback(() => api.list().then(setSessions).catch(() => {}), []);
  useEffect(() => {
    api.config().then(setCfg);
    refresh();
    const t = setInterval(refresh, 5000);
    return () => clearInterval(t);
  }, [refresh]);
  const current = sessionRoute(route);
  return (
    <div className="app">
      <aside>
        <h1>remodev</h1>
        <button className="primary" onClick={() => go("")}>+ New session</button>
        <ul>
          {sessions.map((s) => (
            <li key={s.id} data-testid="session-item" className={s.id === current ? "active" : ""} onClick={() => go("s/" + s.id)}>
              <span className={`dot ${s.busy ? "running" : s.status}`} />
              <div>
                <div className="title">{s.title}</div>
                <div className="meta">{s.repo} · {s.createdBy}</div>
              </div>
            </li>
          ))}
        </ul>
      </aside>
      <main>
        {current ? (
          <SessionView key={current} id={current} onChange={refresh} />
        ) : (
          cfg && <NewSession cfg={cfg} onCreated={(s) => { refresh(); go("s/" + s.id); }} />
        )}
      </main>
    </div>
  );
}

function NewSession({ cfg, onCreated }: { cfg: Config; onCreated: (s: Session) => void }) {
  const [repo, setRepo] = useState(localStorage.getItem("repo") ?? "");
  const [prompt, setPrompt] = useState("");
  const [agent, setAgent] = useState("default");
  const [err, setErr] = useState("");
  const [busy, setBusy] = useState(false);
  const submit = async () => {
    setBusy(true);
    setErr("");
    try {
      localStorage.setItem("repo", repo);
      onCreated(await api.create({ repo, prompt, agent }));
    } catch (e) {
      setErr(String((e as Error).message));
    } finally {
      setBusy(false);
    }
  };
  return (
    <div className="new">
      <h2>New session</h2>
      <label>Repository <small>(allowed: {cfg.allowedRepos.join(", ")})</small>
        <input value={repo} onChange={(e) => setRepo(e.target.value)} placeholder="owner/repo" />
      </label>
      <label>Agent
        <select value={agent} onChange={(e) => setAgent(e.target.value)}>
          {cfg.agents.map((a) => <option key={a.name} value={a.name}>{a.name} — {a.description}</option>)}
        </select>
      </label>
      <label>What should the agent do?
        <textarea rows={8} value={prompt} onChange={(e) => setPrompt(e.target.value)}
          onKeyDown={(e) => { if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) submit(); }} />
      </label>
      {err && <div className="error">{err}</div>}
      <button className="primary" disabled={busy || !repo || !prompt} onClick={submit}>
        {busy ? "Starting…" : "Start (⌘↵)"}
      </button>
    </div>
  );
}

function SessionView({ id, onChange }: { id: string; onChange: () => void }) {
  const [s, setS] = useState<Session>();
  const [events, setEvents] = useState<Event[]>([]);
  const [text, setText] = useState("");
  const [tick, setTick] = useState(0);
  const bottom = useRef<HTMLDivElement>(null);

  useEffect(() => {
    api.get(id).then(setS);
    const es = api.events(id);
    es.onmessage = (m) => {
      const ev: Event = JSON.parse(m.data);
      setEvents((prev) => appendEvent(prev, ev));
      if (ev.type === "status" || ev.type === "pr" || ev.type === "result") {
        api.get(id).then(setS);
        onChange();
      }
      if (ev.type === "artifact" || ev.type === "result" || ev.type === "pr") setTick((t) => t + 1);
    };
    return () => es.close();
  }, [id, onChange]);

  useEffect(() => {
    bottom.current?.scrollIntoView({ block: "end" });
  }, [events.length]);

  if (!s) return null;
  const send = async () => {
    if (!text.trim()) return;
    await api.send(id, text);
    setText("");
  };
  return (
    <div className="session" data-testid="session-view">
      <header>
        <div>
          <h2>{s.title}</h2>
          <div className="meta">
            {s.repo} · <code>{s.branch}</code> ← {s.baseBranch} · {s.status}{s.busy && " (working)"} ·
            {" "}{s.turns} turns · ${s.costUsd.toFixed(2)}
          </div>
        </div>
        <div className="actions">
          {s.prUrl && <a className="button primary" href={s.prUrl} target="_blank">Pull request ↗</a>}
          {s.busy && <button onClick={() => api.interrupt(id)}>Interrupt</button>}
          {s.status !== "stopped" && s.status !== "deleted" && <button onClick={() => api.stop(id)}>Stop</button>}
          {s.status !== "deleted" && (
            <button className="danger" onClick={() => confirm("Delete workspace?") && api.remove(id)}>Delete</button>
          )}
        </div>
      </header>
      <div className="body">
        <div className="left">
          {s.error && <div className="error">{s.error}</div>}
          <div className="log">
            {events.map((e) => <EventRow key={e.seq} e={e} />)}
            <div ref={bottom} />
          </div>
          {s.status !== "deleted" && (
            <div className="composer">
              <textarea rows={3} value={text} placeholder="Send a follow-up (⌘↵)…" onChange={(e) => setText(e.target.value)}
                onKeyDown={(e) => { if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) send(); }} />
              <button className="primary" onClick={send}>Send</button>
            </div>
          )}
        </div>
        <SidePanel s={s} tick={tick} />
      </div>
    </div>
  );
}

function EventRow({ e }: { e: Event }) {
  const time = new Date(e.time).toLocaleTimeString();
  switch (e.type) {
    case "user":
      return <div className="ev user"><b>{e.data?.by ?? "user"}</b><pre>{e.text}</pre></div>;
    case "assistant":
      return <div className="ev assistant"><pre>{e.text}</pre></div>;
    case "result":
      return <div className="ev result"><b>done</b> <span>${(e.data?.costUsd ?? 0).toFixed(3)}</span><pre>{e.text}</pre></div>;
    case "tool_use":
      return (
        <details className="ev tool">
          <summary>🔧 {e.text} <ToolSummary input={e.data?.input} /></summary>
          <pre>{JSON.stringify(e.data?.input, null, 2)}</pre>
        </details>
      );
    case "tool_result":
      return (
        <details className={`ev tool-result ${e.data?.isError ? "err" : ""}`}>
          <summary>↳ {(e.text ?? "").split("\n")[0].slice(0, 120)}</summary>
          <pre>{e.text}</pre>
        </details>
      );
    case "egress":
      return <div className="ev egress">🛡 {e.text}</div>;
    case "artifact":
      return <div className="ev system">📎 {e.text}</div>;
    case "pr":
      return <div className="ev pr">🔀 <a href={e.text} target="_blank">{e.text}</a></div>;
    case "error":
      return <div className="ev error">{e.text}</div>;
    case "status":
      return <div className="ev status">{time} · {e.text}</div>;
    default:
      return <div className="ev system">{e.text}</div>;
  }
}

function ToolSummary({ input }: { input: any }) {
  if (!input || typeof input !== "object") return null;
  const v = input.command ?? input.file_path ?? input.pattern ?? input.url ?? input.description;
  return v ? <code>{String(v).slice(0, 100)}</code> : null;
}

createRoot(document.getElementById("root")!).render(<StrictMode><App /></StrictMode>);
