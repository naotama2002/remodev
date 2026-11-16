// Shared helpers for browser E2E tests: start a control plane binary with
// REMODEV_RUNTIME=fake and open pages that fail on uncaught errors.
import { spawn, type ChildProcess } from "node:child_process";
import { mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { chromium, type Browser, type BrowserContext, type Page } from "playwright-core";

export interface Remodev {
  base: string;
  browser: Browser;
  stop(): Promise<void>;
}

export async function startRemodev(extraEnv: Record<string, string> = {}): Promise<Remodev> {
  const port = 18000 + Math.floor(Math.random() * 1000);
  const base = `http://127.0.0.1:${port}`;
  const server: ChildProcess = spawn(process.env.REMODEV_BIN ?? "../bin/remodev", [], {
    env: {
      ...process.env,
      REMODEV_RUNTIME: "fake",
      REMODEV_LISTEN: `127.0.0.1:${port}`,
      REMODEV_BASE_URL: base,
      REMODEV_DATA_DIR: mkdtempSync(join(tmpdir(), "remodev-e2e-")),
      REMODEV_ALLOWED_REPOS: "acme/*",
      ...extraEnv,
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
  const browser = await chromium.launch({ executablePath: process.env.CHROMIUM_PATH ?? "/usr/bin/chromium", args: ["--no-sandbox"] });
  return {
    base,
    browser,
    async stop() {
      await browser.close();
      server.kill();
    },
  };
}

// open returns a page in a fresh browser context (= its own cookies) that
// records uncaught errors (this is how a blank screen shows up).
export async function open(r: Remodev, path = "/", ctx?: BrowserContext): Promise<{ page: Page; errors: string[] }> {
  const context = ctx ?? (await r.browser.newContext({ viewport: { width: 1400, height: 900 } }));
  const page = await context.newPage();
  const errors: string[] = [];
  page.on("pageerror", (e) => errors.push(e.message));
  await page.goto(r.base + path);
  return { page, errors };
}

export async function startSession(page: Page, prompt: string, opts: { private?: boolean } = {}) {
  await page.getByRole("button", { name: "+ New session" }).click();
  await page.getByPlaceholder("owner/repo").fill("acme/app");
  await page.getByLabel("What should the agent do?").fill(prompt);
  if (opts.private) await page.getByLabel(/非公開にする/).check();
  await page.getByRole("button", { name: /Start/ }).click();
  await page.getByText("完了しました: " + prompt).first().waitFor();
}
