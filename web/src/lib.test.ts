import { test } from "node:test";
import assert from "node:assert/strict";
import { appendEvent, ciState, filterSessions, fmtSize, sessionRoute, splitDiff } from "./lib.ts";

test("splitDiff counts additions and deletions per file", () => {
  const diff = [
    "diff --git a/a.go b/a.go",
    "index 1..2 100644",
    "--- a/a.go",
    "+++ b/a.go",
    "@@ -1,2 +1,2 @@",
    "-old",
    "+new",
    "+more",
    "diff --git a/dir/b.ts b/dir/b.ts",
    "@@ -0,0 +1 @@",
    "+x",
  ].join("\n");
  const files = splitDiff(diff);
  assert.deepEqual(files.map((f) => [f.name, f.add, f.del]), [["a.go", 2, 1], ["dir/b.ts", 1, 0]]);
  assert.deepEqual(files[0].lines, ["@@ -1,2 +1,2 @@", "-old", "+new", "+more"]);
});

test("splitDiff of an empty diff has no files", () => {
  assert.deepEqual(splitDiff(""), []);
});

test("fmtSize", () => {
  assert.equal(fmtSize(10), "10 B");
  assert.equal(fmtSize(2048), "2.0 KB");
  assert.equal(fmtSize(3 * 1024 * 1024), "3.0 MB");
});

test("appendEvent ignores replays and duplicates", () => {
  const ev = (seq: number) => ({ seq, time: "", type: "system" });
  let list = appendEvent([], ev(1));
  list = appendEvent(list, ev(2));
  const same = appendEvent(list, ev(2));
  assert.equal(same, list);
  assert.deepEqual(appendEvent(list, ev(1)).map((e) => e.seq), [1, 2]);
  assert.deepEqual(appendEvent(list, ev(3)).map((e) => e.seq), [1, 2, 3]);
});

test("ciState", () => {
  assert.deepEqual(ciState([]), { kind: "none" });
  assert.deepEqual(ciState([{ status: "completed", conclusion: "success" }, { status: "completed", conclusion: "skipped" }]), { kind: "ok" });
  assert.deepEqual(ciState([{ status: "in_progress", conclusion: "" }, { status: "completed", conclusion: "success" }]), { kind: "pending", count: 1 });
  assert.deepEqual(ciState([{ status: "in_progress", conclusion: "" }, { status: "completed", conclusion: "failure" }]), { kind: "failed", count: 1 });
});

test("sessionRoute", () => {
  assert.equal(sessionRoute("#s/abc123"), "abc123");
  assert.equal(sessionRoute("s/abc123"), "abc123");
  assert.equal(sessionRoute(""), "");
  assert.equal(sessionRoute("#new"), "");
});

test("filterSessions", () => {
  const list = [{ createdBy: "alice" }, { createdBy: "Bob" }];
  assert.equal(filterSessions(list, "all", "bob").length, 2);
  assert.deepEqual(filterSessions(list, "mine", "bob"), [{ createdBy: "Bob" }]);
});
