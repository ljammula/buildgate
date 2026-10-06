// walk.mjs <base-url> <start-token> <walk-dir> [--only <step>...] [--headed]
//
// Drives the React console in Chrome the way an operator would and checks,
// at each step, both that the console let the operator act and that it told
// the truth: after an action the server is asked directly (plain GETs, never
// actions) what it recorded.
//
// Rules the walk holds itself to:
//   - the start token arrives only through the `#t=` link `factoryd serve`
//     prints, and no request may ever carry it in a URL;
//   - every operator action is a click or a keystroke in the page;
//   - any console error, uncaught exception or unexpected 4xx/5xx fails the
//     step it happened in.
//
// Each step prints PASS or FAIL and leaves a screenshot in <walk-dir>/shots.
import fs from "node:fs";
import path from "node:path";

import { chromium } from "playwright";

const [base, token, walkDir, ...flags] = process.argv.slice(2);
if (!base || !token || !walkDir) {
  console.error(
    "usage: walk.mjs <base-url> <start-token> <walk-dir> [--only <step>...] [--headed]",
  );
  process.exit(2);
}
const headed = flags.includes("--headed");
const only = flags.filter((flag, i) => flags[i - 1] === "--only");
const shots = path.join(walkDir, "shots");
fs.mkdirSync(shots, { recursive: true });

const browser = await chromium.launch({ channel: "chrome", headless: !headed });
const context = await browser.newContext({ viewport: { width: 1440, height: 900 } });
const page = await context.newPage();
page.setDefaultTimeout(8000);

// Problems seen since the current step began. `expect4xx` lets a step that
// provokes a refusal on purpose say so.
let problems = [];
let allowed = [];
page.on("console", (message) => {
  if (message.type() !== "error") return;
  const text = message.text();
  if (/favicon/.test(text)) return;
  if (/Failed to load resource/.test(text)) return; // reported with its URL by the response hook
  problems.push(`console error: ${text}`);
});
page.on("pageerror", (error) => problems.push(`uncaught: ${error.message}`));
page.on("request", (request) => {
  if (request.url().includes(token))
    problems.push(`the start token is in a request URL: ${request.url()}`);
});
page.on("response", (response) => {
  const status = response.status();
  if (status < 400) return;
  const url = new URL(response.url());
  if (url.pathname === "/favicon.ico") return;
  const line = `${status} ${response.request().method()} ${url.pathname}`;
  if (allowed.some((pattern) => pattern.test(line))) return;
  problems.push(`unexpected response: ${line}`);
});

/** Ask the server directly; an assertion, never an action. */
async function api(pathname, headers = {}) {
  const response = await fetch(base + pathname, { headers });
  const text = await response.text();
  try {
    return { status: response.status, body: JSON.parse(text) };
  } catch {
    return { status: response.status, body: text };
  }
}
const startHeaders = { Authorization: `Bearer ${token}` };

function check(condition, message) {
  if (!condition) throw new Error(message);
}

async function visit(pathname) {
  await page.goto(base + pathname);
  await page.locator("h1").first().waitFor();
}

const button = (name, options = {}) => page.getByRole("button", { name, ...options });
const link = (name, options = {}) => page.getByRole("link", { name, ...options });
const heading = (name, options = {}) => page.getByRole("heading", { name, ...options });
const dialog = () => page.getByRole("dialog");

/** The operator-name prompt appears before the first write of a session. */
async function nameIfAsked() {
  const field = page.getByLabel("Operator name");
  if (await field.isVisible().catch(() => false)) {
    await field.fill("walk-operator");
    await button("Continue").click();
  }
}

const steps = [];
const step = (name, run) => steps.push({ name, run });
const results = [];

// ---------------------------------------------------------------- steps

step("start-token", async () => {
  await page.goto(`${base}/#t=${token}`);
  await page.getByRole("navigation", { name: "Main" }).waitFor();
  check(!page.url().includes("#t="), `the fragment was not stripped: ${page.url()}`);
  check(!page.url().includes(token), "the token is still in the address bar");
  const stored = await page.evaluate(() => window.localStorage.getItem("factoryStartToken"));
  check(stored === token, "the token was not stored");
  check((await page.evaluate(() => document.referrer)) === "", "document.referrer is not empty");
});

step("shell-navigation", async () => {
  await visit("/");
  for (const [name, pathname, title] of [
    ["Triage", "/triage", "Triage"],
    ["Runs", "/runs", "Factory runs"],
    ["Projects", "/app/projects", "Projects"],
    ["Ops", "/ops", "Operations"],
    ["Requests", "/", "Requests"],
  ]) {
    await page
      .getByRole("navigation", { name: "Main" })
      .getByRole("link", { name, exact: true })
      .click();
    await heading(title, { level: 1 }).waitFor();
    check(new URL(page.url()).pathname === pathname, `${name} went to ${page.url()}`);
  }
  await link("New request").click();
  await heading("New request", { level: 1 }).waitFor();
  await page.goBack();
  await heading("Requests", { level: 1 }).waitFor();
});

step("theme", async () => {
  await visit("/");
  const theme = () => page.evaluate(() => document.documentElement.getAttribute("data-theme"));
  await button("Theme: system (tap for light)").click();
  check((await theme()) === "light", "light was not applied");
  await page.screenshot({ path: path.join(shots, "theme-light.png") });
  await button("Theme: light (tap for dark)").click();
  check((await theme()) === "dark", "dark was not applied");
  await page.reload();
  await button("Theme: dark (tap for system)").waitFor();
  check((await theme()) === "dark", "the choice did not survive a reload");
  await button("Theme: dark (tap for system)").click();
  check((await theme()) === null, "system did not clear the attribute");
});

step("board", async () => {
  await visit("/");
  await heading(/Needs you \(\d+\)/).waitFor();
  await heading(/Working \(\d+\)/).waitFor();
  await heading(/Finished \(\d+\)/).waitFor();
  await page.getByTestId("board-freshness").getByText("Live").waitFor();
  const listed = (await api("/requests")).body;
  for (const request of listed) {
    await page.getByText(request.id, { exact: true }).first().waitFor();
  }
});

step("board-filters", async () => {
  await visit("/");
  await button("Working", { exact: true }).click();
  await page.waitForURL(/group=working/);
  await heading(/Needs you \(/).waitFor({ state: "detached" });
  await heading(/Working \(/).waitFor();
  check(
    (await button("Working", { exact: true }).getAttribute("aria-pressed")) === "true",
    "the Working filter is not shown as on",
  );
  await page.screenshot({ path: path.join(shots, "board-filter-working.png") });
  await button("Working", { exact: true }).click();
  await heading(/Needs you \(/).waitFor();
  check(!page.url().includes("group="), `the filter stayed in the URL: ${page.url()}`);
  await page.getByPlaceholder(/Search/).fill("req-done");
  await page.waitForURL(/q=req-done/);
  await page.getByText("req-done", { exact: true }).waitFor();
  check(
    (await page.getByText("req-building", { exact: true }).count()) === 0,
    "search did not narrow the board",
  );
  // A filtered board is a link: it survives a reload.
  await page.reload();
  await page.getByText("req-done", { exact: true }).waitFor();
  check(
    (await page.getByText("req-building", { exact: true }).count()) === 0,
    "the filter was lost on reload",
  );
});

step("board-refresh", async () => {
  await visit("/");
  const refreshed = page.waitForResponse(
    (r) => new URL(r.url()).pathname === "/requests" && r.request().method() === "GET",
  );
  await button("Refresh").click();
  await refreshed;
});

step("board-opens-request", async () => {
  await visit("/");
  await page
    .getByRole("link", { name: /Add idempotency keys to checkout/ })
    .first()
    .click();
  await page.waitForURL(/\/requests\/req-/);
  await page.locator("h1").first().waitFor();
});

step("runs-list", async () => {
  await visit("/runs");
  for (const id of ["run-accepted", "run-quarantined", "run-running", "run-every-field"]) {
    await page.getByText(id, { exact: true }).first().waitFor();
  }
  await page
    .getByRole("row", { name: /run-accepted/ })
    .getByRole("link")
    .first()
    .click();
  await page.waitForURL(/\/runs\/run-accepted$/);
});

step("run-detail", async () => {
  await visit("/runs/run-accepted");
  await page.getByRole("list", { name: "Timeline stages" }).waitFor();
  await page.getByText("478.3k tokens").first().waitFor();
  await page.getByText("checkout/idempotency.go").first().waitFor();
  // The run links back to its request.
  await link("Open request").click();
  await page.waitForURL(/\/requests\/req-building$/);
});

step("run-diff", async () => {
  await visit("/runs/run-accepted");
  await page.getByRole("tab", { name: "View diff" }).click();
  await page.waitForURL(/view=diff/);
  await page.getByText("+// Key scopes a retry to one account.").waitFor();
  // The view is a link: a reload lands on it.
  await page.reload();
  await page.getByText("+// Key scopes a retry to one account.").waitFor();
});

step("run-release", async () => {
  await visit("/runs/run-accepted?view=release");
  await page
    .getByText(/kill switch is engaged for project "app"/)
    .first()
    .waitFor();
  await page.getByText("incident 42").first().waitFor();
});

step("run-log", async () => {
  await visit("/runs/run-accepted");
  await page.getByRole("switch", { name: /Show live build log/ }).click();
  await page.getByText("round 1: verify passed").waitFor();
  await page.getByRole("switch", { name: /Show live build log/ }).click();
  check(
    (await page.getByText("round 1: verify passed").count()) === 0,
    "the log stayed after the pane was closed",
  );
});

step("run-quarantined", async () => {
  await visit("/runs/run-quarantined");
  await heading("Operator override").waitFor();
  await button("Override run").waitFor();
  check(
    (await page.getByRole("tab", { name: "View diff" }).count()) === 0,
    "a run with no diff offers a diff view",
  );
});

step("projects", async () => {
  await visit("/app/projects");
  await page.getByText("app", { exact: true }).first().waitFor();
});

step("project-stats", async () => {
  await visit("/projects/app/stats");
  await page
    .getByText(/every-field|Accepted|accepted/)
    .first()
    .waitFor();
  const stats = (await api("/projects/app/stats", startHeaders)).body;
  await page.getByText(String(stats.total_runs), { exact: true }).first().waitFor();
});

step("project-release", async () => {
  await visit("/projects/app/release");
  await page.getByText("incident 42").first().waitFor();
  await page.getByText("operator@example.com").first().waitFor();
});

step("ops", async () => {
  await visit("/ops");
  await page.getByText("app", { exact: true }).first().waitFor();
  const refreshed = page.waitForResponse((r) => new URL(r.url()).pathname === "/projects");
  await button("Refresh").click();
  await refreshed;
});

step("new-request-refused", async () => {
  // This workspace has no .factory.yml and no verify command is given: the
  // server refuses, and its reason must be on screen without a click.
  await visit("/requests/new");
  allowed = [/^422 POST \/requests$/];
  await page.getByLabel("Workspace path").fill(path.join(walkDir, "workspace"));
  await page.getByLabel("Request", { exact: true }).fill("Add a healthz endpoint");
  await button("Submit request").click();
  await nameIfAsked();
  await page
    .getByRole("alert")
    .getByText(/no verify command resolvable/)
    .waitFor();
});

step("new-request", async () => {
  await visit("/requests/new");
  const before = (await api("/requests")).body.length;
  const workspace = path.join(walkDir, "workspace");
  await page.getByLabel("Workspace path").fill(workspace);
  await page
    .getByLabel("Request", { exact: true })
    .fill("Add a healthz endpoint\n\nIt returns 200 and the build SHA.");
  await page.getByLabel("Draft oracles").check();
  await button("Advanced").click();
  await page.getByLabel(/Verify command/).fill("go test ./...");
  // An empty repository has none of the project artifacts the default
  // preflight requires.
  await page.getByLabel(/Preflight profile/).fill("brownfield");
  await button("Submit request").click();
  await nameIfAsked();
  await page.waitForURL(/\/requests\/(?!new$)[^/]+$/);
  const id = decodeURIComponent(new URL(page.url()).pathname.split("/").pop());
  const created = await api(`/requests/${id}`);
  check(created.status === 200, `the server has no request ${id}`);
  check(
    created.body.verify_command === "go test ./...",
    `verify_command recorded as ${created.body.verify_command}`,
  );
  check(created.body.draft_oracles === true, "draft_oracles was not recorded");
  check(
    (await api("/requests")).body.length === before + 1,
    "the request list did not grow by one",
  );
  // The new request is on the board without a reload of the board's data by hand.
  await page
    .getByRole("navigation", { name: "Main" })
    .getByRole("link", { name: "Requests", exact: true })
    .click();
  await page.getByText(id, { exact: true }).first().waitFor();
});

step("new-run-check", async () => {
  // The project check reports each failing artifact; nothing is started.
  await visit("/app/runs/new");
  allowed = [/^(4|5)\d\d POST \/projects\/check$/];
  const fields = page.getByRole("textbox");
  check((await fields.count()) >= 3, "the new-run form has fewer fields than expected");
  await page.screenshot({ path: path.join(shots, "new-run-empty.png"), fullPage: true });
  await button("Check project setup").click();
  await page.waitForTimeout(500);
});

step("triage-keys", async () => {
  await visit("/triage");
  await button(/Approve \(a\)/).waitFor();
  await page.keyboard.press("j");
  await page.keyboard.press("k");
  await page.keyboard.press("r");
  await dialog().waitFor();
  await nameIfAsked();
  await dialog().getByRole("button", { name: "Cancel" }).click();
  await dialog().waitFor({ state: "detached" });
});

// ---------------------------------------------------------------- runner

for (const { name, run } of steps) {
  if (only.length > 0 && !only.includes(name)) continue;
  problems = [];
  allowed = [];
  let failure = null;
  try {
    await run();
    await page.waitForTimeout(150);
    if (problems.length > 0) failure = problems.join("\n      ");
  } catch (error) {
    failure = [error.message.split("\n")[0], ...problems].join("\n      ");
  }
  await page
    .screenshot({ path: path.join(shots, `${name}.png`), fullPage: true })
    .catch(() => undefined);
  results.push({ name, failure });
  console.log(failure ? `FAIL  ${name}\n      ${failure}` : `PASS  ${name}`);
  if (failure) {
    const controls = await page
      .getByRole("button")
      .or(page.getByRole("link"))
      .or(page.getByRole("tab"))
      .or(page.getByRole("textbox"))
      .evaluateAll((nodes) =>
        nodes
          .slice(0, 60)
          .map(
            (n) =>
              `${n.getAttribute("role") ?? n.tagName.toLowerCase()}:${(n.getAttribute("aria-label") ?? n.textContent ?? "").trim().slice(0, 40)}`,
          ),
      )
      .catch(() => []);
    console.log(`      at ${page.url()}\n      controls: ${controls.join(" | ")}`);
  }
}

await browser.close();
const failed = results.filter((result) => result.failure);
console.log(
  `\n${results.length - failed.length} of ${results.length} steps passed. Screenshots: ${shots}`,
);
process.exit(failed.length === 0 ? 0 : 1);
