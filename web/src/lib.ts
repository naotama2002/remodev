// Pure helpers used by the UI. No React, no DOM: unit-tested with `node --test`.
import type { Event } from "./api.ts";

export interface DiffFile {
  name: string;
  add: number;
  del: number;
  lines: string[];
}

const maxLinesPerFile = 2000;

// splitDiff splits a unified diff into files with added/removed line counts.
export function splitDiff(diff: string): DiffFile[] {
  const files: DiffFile[] = [];
  let cur: DiffFile | undefined;
  for (const line of diff.split("\n")) {
    if (line.startsWith("diff --git ")) {
      const m = / b\/(.*)$/.exec(line);
      cur = { name: m ? m[1] : line, add: 0, del: 0, lines: [] };
      files.push(cur);
      continue;
    }
    if (!cur || line.startsWith("index ") || line.startsWith("--- ") || line.startsWith("+++ ")) continue;
    if (line.startsWith("+")) cur.add++;
    else if (line.startsWith("-")) cur.del++;
    if (cur.lines.length < maxLinesPerFile) cur.lines.push(line);
  }
  return files;
}

export function fmtSize(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1 << 20) return `${(n / 1024).toFixed(1)} KB`;
  return `${(n / (1 << 20)).toFixed(1)} MB`;
}

// appendEvent adds an event from the SSE stream, ignoring duplicates and
// replays (EventSource reconnects resend events after Last-Event-ID, and the
// list must stay ordered by seq).
export function appendEvent(list: Event[], ev: Event): Event[] {
  if (list.length > 0 && list[list.length - 1].seq >= ev.seq) return list;
  return [...list, ev];
}

export type CIState = { kind: "none" } | { kind: "pending"; count: number } | { kind: "failed"; count: number } | { kind: "ok" };

// ciState summarizes GitHub check runs.
export function ciState(runs: { status: string; conclusion: string }[]): CIState {
  if (runs.length === 0) return { kind: "none" };
  const failed = runs.filter((r) => r.status === "completed" && !["success", "neutral", "skipped"].includes(r.conclusion)).length;
  if (failed) return { kind: "failed", count: failed };
  const pending = runs.filter((r) => r.status !== "completed").length;
  if (pending) return { kind: "pending", count: pending };
  return { kind: "ok" };
}

// sessionRoute parses the location hash: "#s/<id>" -> id, anything else -> "".
export function sessionRoute(hash: string): string {
  const h = hash.replace(/^#/, "");
  return h.startsWith("s/") ? h.slice(2) : "";
}
