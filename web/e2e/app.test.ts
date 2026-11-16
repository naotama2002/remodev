// Browser end-to-end tests of the UI against a real control plane binary
// running with REMODEV_RUNTIME=fake (no Docker / GitHub / Claude needed).
//
//   REMODEV_BIN=/path/to/remodev CHROMIUM_PATH=/usr/bin/chromium pnpm test:e2e
//
// Tests talk to the UI like a user (roles, labels, visible text) and assert
// on what the user sees, so they survive markup/CSS changes.
import { after, before, test } from "node:test";
import assert from "node:assert/strict";
import { spawn, type ChildProcess } from "node:child_process";
import { mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { chromium, type Browser, type Page } from "playwright-core";

const bin = process.env.REMODEV_BIN ?? "../bin/remodev";
const port = 18000 + Math.floor(Math.random() * 1000);
const base = `http://127.0.0.1:${port}`;
let server: ChildProcess;
let browser: Browser;

before(async () => {
  server = spawn(bin, [], {
    env: {
      ...process.env,
      REMODEV_RUNTIME: "fake",
      REMODEV_LISTEN: `127.0.0.1:${port}`,
      REMODEV_DATA_DIR: mkdtempSync(join(tmpdir(), "remodev-e2e-")),
      REMODEV_ALLOWED_REPOS: "acme/*",
    },
    stdio: ["ignore", "ignore", "inherit"],
  });
  for (let i = 0; ; i++) {
    try {
      if ((await fetch(base + "/healthz")).ok) break;
    } catch {}
    if (i > 100) throw new Error("control plane did not start");
    await new Promise((r) => setTimeout(r, 100));
  }
  browser = await chromium.launch({ executablePath: process.env.CHROMIUM_PATH ?? "/usr/bin/chromium", args: ["--no-sandbox"] });
});

after(async () => {
  await browser?.close();
  server?.kill();
});

// Every page fails the test on uncaught errors (this is how a blank screen shows up).
async function open(path = "/"): Promise<{ page: Page; errors: string[] }> {
  const page = await browser.newPage({ viewport: { width: 1400, height: 900 } });
  const errors: string[] = [];
  page.on("pageerror", (e) => errors.push(e.message));
  await page.goto(base + path);
  return { page, errors };
}

async function startSession(page: Page, prompt: string) {
  await page.getByRole("button", { name: "+ New session" }).click();
  await page.getByPlaceholder("owner/repo").fill("acme/app");
  await page.getByLabel("What should the agent do?").fill(prompt);
  await page.getByRole("button", { name: /Start/ }).click();
  await page.getByText("完了しました: " + prompt).first().waitFor();
}

test("start a session from the UI and see the result, PR and screenshot", async () => {
  const { page, errors } = await open();
  await startSession(page, "add a sort button");

  await page.getByRole("heading", { name: "add a sort button" }).waitFor();
  await page.getByRole("link", { name: "Pull request ↗" }).waitFor();
  await page.getByRole("img", { name: "demo-screenshot.png" }).waitFor();

  await page.getByRole("tab", { name: "差分" }).click();
  const diff = page.getByRole("tabpanel");
  await diff.getByText("README.md", { exact: true }).waitFor();
  await diff.getByText("+add a sort button").waitFor();

  await page.getByPlaceholder(/Send a follow-up/).fill("also add tests");
  await page.getByRole("button", { name: "Send" }).click();
  await page.getByText("完了しました: also add tests").first().waitFor();

  assert.deepEqual(errors, []);
  await page.close();
});

test("switching between sessions never blanks the screen", async () => {
  const { page, errors } = await open();
  await startSession(page, "first");
  await startSession(page, "second");
  await startSession(page, "third");

  const items = page.getByTestId("session-item");
  const n = await items.count();
  assert.ok(n >= 3);
  for (let round = 0; round < 3; round++) {
    for (let i = 0; i < n; i++) {
      await items.nth(i).click();
      await page.getByTestId("session-view").waitFor({ timeout: 3000 });
    }
  }
  assert.deepEqual(errors, []);
  await page.close();
});

test("reloading a session URL restores the conversation", async () => {
  const { page, errors } = await open();
  await startSession(page, "persist me");
  const url = page.url();
  const reloaded = await browser.newPage();
  await reloaded.goto(url);
  await reloaded.getByText("完了しました: persist me").first().waitFor();
  assert.deepEqual(errors, []);
  await page.close();
  await reloaded.close();
});

test("shows a validation error for a repository that is not allowed", async () => {
  const { page, errors } = await open();
  await page.getByPlaceholder("owner/repo").fill("evil/app");
  await page.getByLabel("What should the agent do?").fill("x");
  await page.getByRole("button", { name: /Start/ }).click();
  await page.getByText(/not in REMODEV_ALLOWED_REPOS/).waitFor();
  assert.deepEqual(errors, []);
  await page.close();
});
