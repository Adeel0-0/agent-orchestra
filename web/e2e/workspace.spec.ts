import { test, expect, type WebSocketRoute } from "@playwright/test";
import { spawn, type ChildProcessWithoutNullStreams } from "node:child_process";
import { resolve } from "node:path";
let child: ChildProcessWithoutNullStreams, url: string;
const fixtureErrors: string[] = [];
test.beforeAll(async () => {
  child = spawn(
    resolve("../.orchestra/phase3-venv/Scripts/python.exe"),
    [resolve("../tests/ui_fixture.py")],
    {
      cwd: resolve(".."),
      env: { ...process.env, PYTHONPATH: resolve("..") },
      stdio: "pipe",
    },
  );
  url = await new Promise<string>((yes, no) => {
    let output = "";
    const timer = setTimeout(
      () => no(new Error("Fixture server timed out")),
      15000,
    );
    child.stdout.on("data", (chunk) => {
      output += chunk;
      const match = output.match(/ORCHESTRA_E2E_READY (.+)\r?\n/);
      if (match) {
        clearTimeout(timer);
        yes(JSON.parse(match[1]).url);
      }
    });
    child.stderr.on("data", (chunk) => fixtureErrors.push(String(chunk)));
    child.once("exit", (code) => {
      clearTimeout(timer);
      no(new Error(`Fixture exited ${code}`));
    });
  });
});
test.afterAll(async () => {
  if (child && !child.killed) {
    const exit = new Promise<number | null>((resolve) =>
      child.once("exit", resolve),
    );
    child.stdin.end("stop\n");
    expect(await exit, fixtureErrors.join("\n")).toBe(0);
    expect(fixtureErrors).toEqual([]);
  }
});
test("cold start, folder selection, approval, diff, model switch and themes", async ({
  page,
}) => {
  const browserErrors: string[] = [];
  page.on("pageerror", (error) => browserErrors.push(error.message));
  let socket: WebSocketRoute;
  let serverSocket: WebSocketRoute;
  const servers: WebSocketRoute[] = [];
  const connections: string[] = [];
  await page.routeWebSocket(/\/api\/sessions\/.*\/events/, (ws) => {
    socket = ws;
    connections.push(ws.url());
    serverSocket = ws.connectToServer();
    servers.push(serverSocket);
  });
  await page.goto(url);
  await expect(
    page.getByRole("heading", { name: "Open a project to start" }),
  ).toBeVisible();
  await expect(page).not.toHaveURL(/bootstrap=/);
  await page
    .getByRole("button", { name: "Open folder", exact: true })
    .first()
    .click();
  await expect(
    page.getByText("Sample project", { exact: true }).first(),
  ).toBeVisible();
  await page
    .getByRole("button", { name: "New session", exact: true })
    .first()
    .click();
  await page.getByLabel("Session agent").selectOption("codex");
  await page.getByLabel("Session permissions").selectOption("read-only");
  await page.getByRole("button", { name: "Create session" }).click();
  await expect(page.getByRole("dialog")).not.toBeVisible();
  await page
    .getByRole("textbox", { name: "Message" })
    .fill("Change the greeting to goodbye");
  await expect(page.getByRole("textbox", { name: "Message" })).toHaveValue(
    "Change the greeting to goodbye",
  );
  await expect(
    page.getByRole("button", { name: "Send message" }),
  ).toBeEnabled();
  await page.getByRole("button", { name: "Send message" }).click();
  await expect(page.getByRole("button", { name: "Allow edit" })).toBeVisible();
  await page.getByRole("button", { name: "Allow edit" }).click();
  await expect(
    page.getByText("The file now prints", { exact: false }),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: /greeting.py/ }).first(),
  ).toBeVisible();
  await page
    .getByRole("button", { name: /greeting.py/ })
    .first()
    .click();
  await expect(
    page.getByText('+print("goodbye")', { exact: false }),
  ).toBeVisible();
  await page.screenshot({
    path: "test-results/workspace-light.png",
    fullPage: true,
  });
  // Drop the live socket and verify journal replay does not duplicate the turn.
  const count = connections.length;
  await socket!.close({ code: 1012, reason: "Offline acceptance disconnect" });
  await serverSocket!.close();
  await expect.poll(() => connections.length).toBe(count + 1);
  expect(new URL(connections.at(-1)!).searchParams.get("after")).not.toBe("0");
  await expect(
    page.getByText("The file now prints", { exact: false }),
  ).toHaveCount(1);
  await page.getByLabel("Model", { exact: true }).fill("fixture-large");
  await page
    .getByRole("textbox", { name: "Message" })
    .fill("Another test on the selected model");
  await page.getByRole("button", { name: "Send message" }).click();
  await expect(page.getByRole("button", { name: "Allow edit" })).toBeVisible();
  await page.getByRole("button", { name: "Deny", exact: true }).click();
  await expect(
    page.getByText("No files changed.", { exact: true }),
  ).toBeVisible();
  // A delayed response for the old session must not overwrite the new view.
  const firstSession = new URL(connections[0]).pathname.split("/")[3];
  let releaseOld!: () => void;
  let observedOld!: () => void;
  const delayed = new Promise<void>((resolve) => {
    releaseOld = resolve;
  });
  const requested = new Promise<void>((resolve) => {
    observedOld = resolve;
  });
  await page.route(`**/api/sessions/${firstSession}/diff`, async (route) => {
    observedOld();
    await delayed;
    await route.fulfill({
      json: {
        base_commit: "old",
        diff: "diff --git a/stale.py b/stale.py\n+WRONG OLD VIEW\n",
        untracked: [],
        truncated: false,
      },
    });
  });
  await page
    .getByRole("button", {
      name: "Change the greeting to goodbye",
      exact: true,
    })
    .click();
  await requested;
  await page
    .getByRole("button", {
      name: "Another test on the selected model",
      exact: true,
    })
    .click();
  await expect(
    page.getByText("No files changed.", { exact: true }),
  ).toBeVisible();
  const oldResponse = page.waitForResponse((response) =>
    response.url().endsWith(`/sessions/${firstSession}/diff`),
  );
  releaseOld();
  await oldResponse;
  await expect(
    page.getByRole("button", { name: /greeting.py/ }).first(),
  ).toBeVisible();
  await expect(page.getByText("WRONG OLD VIEW", { exact: false })).toHaveCount(
    0,
  );
  await page.unroute(`**/api/sessions/${firstSession}/diff`);
  await page.getByRole("button", { name: "Settings", exact: true }).click();
  await page.getByLabel("Appearance").selectOption("dark");
  await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
  await expect(page.locator("body")).toHaveCSS("color", "rgb(227, 234, 244)");
  await page.getByRole("button", { name: "Close settings" }).click();
  await page.screenshot({
    path: "test-results/workspace-dark.png",
    fullPage: true,
  });
  await page.setViewportSize({ width: 820, height: 900 });
  await expect(
    page.getByRole("complementary", { name: "Changes", exact: true }),
  ).not.toBeVisible();
  await expect(page.getByRole("textbox", { name: "Message" })).toBeVisible();
  const width = await page.evaluate(() => document.documentElement.scrollWidth);
  expect(width).toBeLessThanOrEqual(820);
  await page.screenshot({
    path: "test-results/workspace-narrow.png",
    fullPage: true,
  });
  const storage = await page.evaluate(() => ({
    local: Object.keys(localStorage),
    session: Object.keys(sessionStorage),
  }));
  expect(storage.local).toEqual(["orchestra-theme"]);
  expect(storage.session).toEqual([]);
  await page.reload();
  await expect(
    page.getByRole("heading", { name: "Reconnect to your workspace" }),
  ).toBeVisible();
  expect(browserErrors).toEqual([]);
  await page.close();
  await Promise.all(servers.map((server) => server.close()));
});
