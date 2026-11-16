// Login and session visibility, with two users in separate browsers.
import { after, before, test } from "node:test";
import assert from "node:assert/strict";
import type { Page } from "playwright-core";
import { open, startRemodev, startSession, type Remodev } from "./helpers.ts";

let r: Remodev;
before(async () => {
  r = await startRemodev({ REMODEV_AUTH: "github", REMODEV_ALLOWED_ORGS: "acme" });
});
after(async () => {
  await r?.stop();
});

async function login(name: string): Promise<{ page: Page; errors: string[] }> {
  const o = await open(r);
  await o.page.getByLabel("ユーザー名").fill(name);
  await o.page.getByRole("button", { name: "ログイン" }).click();
  await o.page.getByRole("button", { name: "ログアウト" }).waitFor();
  return o;
}

test("a team session is visible to teammates; a private one is not", async () => {
  const alice = await login("alice");
  await startSession(alice.page, "team work");
  await startSession(alice.page, "secret work", { private: true });

  const bob = await login("bob");
  const list = bob.page.getByTestId("session-item");
  await bob.page.getByText("team work").waitFor();
  assert.equal(await bob.page.getByText("secret work").count(), 0);

  // Bob can follow up on the team session but cannot delete it.
  await list.filter({ hasText: "team work" }).click();
  await bob.page.getByText("作成: alice").waitFor();
  assert.equal(await bob.page.getByRole("button", { name: "Delete" }).count(), 0);
  await bob.page.getByPlaceholder(/Send a follow-up/).fill("bob here");
  await bob.page.getByRole("button", { name: "Send" }).click();
  await bob.page.getByText("完了しました: bob here").first().waitFor();

  // Alice publishes the private session; Bob now sees it.
  await alice.page.getByTestId("session-item").filter({ hasText: "secret work" }).click();
  await alice.page.getByRole("button", { name: /チームに公開/ }).click();
  await bob.page.reload();
  await bob.page.getByText("secret work").waitFor();

  assert.deepEqual([...alice.errors, ...bob.errors], []);
});

test("the session URL of a private session does not leak it", async () => {
  const alice = await login("carol");
  await startSession(alice.page, "carol private", { private: true });
  const url = alice.page.url();
  const dave = await login("dave");
  await dave.page.goto(url);
  await dave.page.getByRole("heading", { name: "セッションが見つかりません" }).waitFor();
  assert.equal(await dave.page.getByText("carol private").count(), 0);
  assert.deepEqual(dave.errors, []);
});

test("users outside the organization are rejected", async () => {
  const { page } = await open(r);
  await page.getByLabel("ユーザー名").fill("outsider-eve");
  await page.getByRole("button", { name: "ログイン" }).click();
  await page.getByText(/remodev を使う権限がありません/).waitFor();
});
