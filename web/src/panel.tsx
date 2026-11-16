import { useEffect, useState } from "react";
import { api, type Artifact, type Checks, type Diff, type Session } from "./api";
import { ciState, fmtSize, splitDiff } from "./lib";

type Tab = "artifacts" | "diff" | "preview";

// Right-hand side panel of the session view: artifacts, diff, PR/CI, preview.
export function SidePanel({ s, tick }: { s: Session; tick: number }) {
  const [tab, setTab] = useState<Tab>("artifacts");
  return (
    <div className="side">
      <div className="tabs" role="tablist">
        {(["artifacts", "diff", "preview"] as Tab[]).map((t) => (
          <span key={t} role="tab" aria-selected={tab === t} className={tab === t ? "on" : ""} onClick={() => setTab(t)}>
            {{ artifacts: "成果物", diff: "差分", preview: "プレビュー" }[t]}
          </span>
        ))}
      </div>
      <div role="tabpanel">
        {tab === "artifacts" && <ArtifactsTab s={s} tick={tick} />}
        {tab === "diff" && <DiffTab s={s} tick={tick} />}
        {tab === "preview" && <PreviewTab s={s} />}
      </div>
    </div>
  );
}

function ArtifactsTab({ s, tick }: { s: Session; tick: number }) {
  const [list, setList] = useState<Artifact[]>([]);
  const [checks, setChecks] = useState<Checks>();
  const [open, setOpen] = useState<string>();
  useEffect(() => {
    api.artifacts(s.id).then(setList).catch(() => {});
  }, [s.id, tick]);
  useEffect(() => {
    api.checks(s.id).then(setChecks).catch(() => {});
  }, [s.id, tick, s.prUrl]);
  const images = list.filter((a) => a.type.startsWith("image/"));
  const others = list.filter((a) => !a.type.startsWith("image/"));
  return (
    <>
      <section>
        <h4>Pull Request</h4>
        {checks?.pr ? (
          <div className="kv">
            <span>PR</span>
            <span>
              <a href={checks.pr.html_url} target="_blank">#{checks.pr.number}</a>{" "}
              {checks.pr.merged_at ? "merged" : checks.pr.draft ? "draft" : checks.pr.state}
            </span>
            <span>CI</span>
            <span>{checks.runs.length === 0 ? <span className="dim">なし</span> : <CI runs={checks.runs} />}</span>
          </div>
        ) : (
          <div className="dim">まだありません</div>
        )}
      </section>
      <section>
        <h4>スクリーンショット ({images.length})</h4>
        {images.length === 0 && <div className="dim">まだありません</div>}
        <div className="shots">
          {images.map((a) => (
            <figure key={a.name + a.modTime} onClick={() => setOpen(a.name)}>
              <img src={api.artifactUrl(s.id, a.name) + "?v=" + Date.parse(a.modTime)} alt={a.name} loading="lazy" />
              <figcaption>{a.name}</figcaption>
            </figure>
          ))}
        </div>
      </section>
      {others.length > 0 && (
        <section>
          <h4>ファイル ({others.length})</h4>
          <ul className="files">
            {others.map((a) => (
              <li key={a.name}>
                <a href={api.artifactUrl(s.id, a.name)} target="_blank">{a.name}</a>{" "}
                <span className="dim">{fmtSize(a.size)}</span>
              </li>
            ))}
          </ul>
        </section>
      )}
      {open && (
        <div className="lightbox" onClick={() => setOpen(undefined)}>
          <img src={api.artifactUrl(s.id, open)} alt={open} />
        </div>
      )}
    </>
  );
}

function CI({ runs }: { runs: Checks["runs"] }) {
  const st = ciState(runs);
  const cls = st.kind === "failed" ? "r" : st.kind === "pending" ? "y" : "g";
  const label = st.kind === "failed" ? `✗ ${st.count} 失敗` : st.kind === "pending" ? `● ${st.count} 実行中` : "✓ 成功";
  return (
    <details className="ci">
      <summary><span className={`chip ${cls}`}>{label}</span> <span className="dim">{runs.length} checks</span></summary>
      <ul>
        {runs.map((r) => (
          <li key={r.name + r.html_url}>
            <a href={r.html_url} target="_blank">{r.name}</a> <span className="dim">{r.status === "completed" ? r.conclusion : r.status}</span>
          </li>
        ))}
      </ul>
    </details>
  );
}

function DiffTab({ s, tick }: { s: Session; tick: number }) {
  const [d, setD] = useState<Diff>();
  const [err, setErr] = useState("");
  useEffect(() => {
    api.diff(s.id).then((x) => { setD(x); setErr(""); }).catch((e) => setErr(String(e.message)));
  }, [s.id, tick]);
  if (err) return <section className="error">{err}</section>;
  if (!d) return <section className="dim">読み込み中…</section>;
  if (d.source === "none") return <section className="dim">まだ push されていません</section>;
  const files = splitDiff(d.diff);
  return (
    <>
      <section className="dim sm">
        {d.source === "sandbox" ? "サンドボックスの作業ツリー（未コミットを含む）" : "GitHub 上のブランチ"} · {files.length} ファイル
        {d.truncated && " · 一部省略"}
      </section>
      {d.untracked.length > 0 && (
        <section>
          <h4>未追跡 ({d.untracked.length})</h4>
          <ul className="files">{d.untracked.map((f) => <li key={f}><code>{f}</code></li>)}</ul>
        </section>
      )}
      {files.map((f) => (
        <details key={f.name} className="difffile" open={files.length <= 5}>
          <summary>
            <code>{f.name}</code> <span className="add">+{f.add}</span> <span className="del">-{f.del}</span>
          </summary>
          <pre className="diff">
            {f.lines.map((l, i) => (
              <div key={i} className={l.startsWith("+") ? "add" : l.startsWith("-") ? "del" : l.startsWith("@@") ? "hunk" : ""}>{l || " "}</div>
            ))}
          </pre>
        </details>
      ))}
    </>
  );
}

function PreviewTab({ s }: { s: Session }) {
  const [port, setPort] = useState(localStorage.getItem("previewPort") ?? "3000");
  const url = s.previewBase.replace("{port}", port);
  return (
    <section>
      <h4>プレビュー</h4>
      <div className="kv">
        <span>ポート</span>
        <span><input value={port} size={6} onChange={(e) => { setPort(e.target.value); localStorage.setItem("previewPort", e.target.value); }} /></span>
        <span>URL</span>
        <span><a href={url} target="_blank">{url} ↗</a></span>
      </div>
      <p className="dim sm">サンドボックス内で 0.0.0.0 に bind したサーバーを開けます（例: <code>remodev-bg npm run dev -- --host 0.0.0.0</code>）。</p>
    </section>
  );
}
