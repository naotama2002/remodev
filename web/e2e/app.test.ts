// Browser end-to-end tests of the UI against a real control plane binary
// running with REMODEV_RUNTIME=fake (no Docker / GitHub / Claude needed).
// They act like a user (roles, labels, visible text) so markup and CSS can
// change freely.
import { after, before, test } from "node:test";
import assert from "node:assert/strict";
import { open, startRemodev, startSession, type Remodev } from "./helpers.ts";

let r: Remodev;
before(async () => {
  r = await startRemodev();
});
after(async () => {
  await r?.stop();
});

test("start a session from the UI and see the result, PR and screenshot", async () => {
  const { page, errors } = await open(r);
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
  const { page, errors } = await open(r);
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
  const { page, errors } = await open(r);
  await startSession(page, "persist me");
  const url = page.url();
  const reloaded = await r.browser.newPage();
  await reloaded.goto(url);
  await reloaded.getByText("完了しました: persist me").first().waitFor();
  assert.deepEqual(errors, []);
  await page.close();
  await reloaded.close();
});

test("shows a validation error for a repository that is not allowed", async () => {
  const { page, errors } = await open(r);
  await page.getByPlaceholder("owner/repo").fill("evil/app");
  await page.getByLabel("What should the agent do?").fill("x");
  await page.getByRole("button", { name: /Start/ }).click();
  await page.getByText(/not in REMODEV_ALLOWED_REPOS/).waitFor();
  assert.deepEqual(errors, []);
  await page.close();
});
