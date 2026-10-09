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
import { createHash } from "node:crypto";
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

// Playwright's own Chromium by default (a pinned browser, the same on every
// machine); WALK_BROWSER_CHANNEL=chrome uses an installed Google Chrome.
const channel = process.env.WALK_BROWSER_CHANNEL;
const browser = await chromium.launch({ ...(channel ? { channel } : {}), headless: !headed });
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

const sha256 = (text) => createHash("sha256").update(text, "utf8").digest("hex");

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
  // The run left a handoff: the page lists what the server's record lists.
  const handoff = (await api("/runs/run-quarantined/handoff")).body;
  const card = page.getByTestId("run-handoff");
  for (const failed of handoff.checks) {
    await card.getByText(failed.check, { exact: true }).waitFor();
    if (failed.finding) await card.getByText(failed.finding, { exact: true }).waitFor();
  }
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
  // A row opens in place: its figures and its kill switch are two tabs.
  await main()
    .getByRole("button", { name: /^Show details for / })
    .first()
    .click();
  const stats = (await api("/projects/app/stats", startHeaders)).body;
  await main().getByRole("tab", { name: "Stats", exact: true }).waitFor();
  await main()
    .getByRole("tabpanel")
    .getByText(String(stats.total_runs), { exact: true })
    .first()
    .waitFor();
  await main().getByRole("tab", { name: "Release", exact: true }).click();
  await main().getByRole("tabpanel").getByText("incident 42").first().waitFor();
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

step("project-observations", async () => {
  await visit("/projects/app/observations");
  // The page says what the server's own report says about the seeded runs.
  const report = (await api("/projects/app/observations")).body;
  await page.getByText(report.observations[0].what).first().waitFor();
  await page.getByText("idempotency_test.go:41: key reused across accounts").first().waitFor();
  await page.getByRole("link", { name: report.observations[0].run_id }).first().waitFor();
});

step("project-release", async () => {
  await visit("/projects/app/release");
  await page.getByText("incident 42").first().waitFor();
  await page.getByText("operator@example.com").first().waitFor();
});

step("ops", async () => {
  await visit("/ops");
  await page.getByText("app", { exact: true }).first().waitFor();
  // The walk starts no worker, and the card says what the server's own route says.
  const worker = (await api("/queue-run")).body;
  await page
    .getByRole("region", { name: "Daemons" })
    .getByText(
      worker.state === "alive" ? "Running" : worker.state === "stale" ? "Stale" : "Not running",
      {
        exact: true,
      },
    )
    .waitFor();
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
  // Triage lists everything that needs the operator. What it cannot decide
  // in place links to the request page; a spec or plan is decided here.
  const elsewhere = page.getByRole("link", { name: "Open request" });
  const inPlace = button(/Approve \(a\)/);
  await elsewhere.or(inPlace).first().waitFor();
  const count = await page
    .getByRole("navigation", { name: "Main" })
    .getByRole("link", { name: /^Triage/ })
    .innerText();
  check(/\d/.test(count), `the sidebar shows no Triage count: ${count}`);
  const specRow = page
    .getByRole("main")
    .getByRole("button", { name: /Spec review/ })
    .first();
  await specRow.click();
  await inPlace.waitFor();
  await page.keyboard.press("j");
  await page.keyboard.press("k");
  await specRow.click();
  await page.keyboard.press("r");
  await dialog().waitFor();
  await nameIfAsked();
  await dialog().getByRole("button", { name: "Cancel" }).click();
  await dialog().waitFor({ state: "detached" });
});

const main = () => page.getByRole("main");
const request = async (id) => (await api(`/requests/${id}`)).body;

/** Confirm the dialog that is open with its button, and wait for it to close. */
async function confirmDialog(name) {
  await nameIfAsked();
  await dialog().getByRole("button", { name, exact: true }).click();
  await dialog().waitFor({ state: "detached" });
}

step("request-page", async () => {
  await visit("/requests/req-spec-review");
  await heading("Add idempotency keys to checkout", { level: 1 }).waitFor();
  await page.getByText("Acceptance criteria (2)").waitFor();
  await page.getByText("A key is scoped to one account.").first().waitFor();
  // Rendered and raw views of the same spec.
  await page.getByLabel("Raw").uncheck();
  await heading("Idempotency keys for checkout").waitFor();
  await page.getByLabel("Raw").check();
  await link("Back to board").click();
  await heading("Requests", { level: 1 }).waitFor();
});

const validSpec = `# Spec

## Problem

A retried checkout charges the card twice.

## Scope

The checkout endpoint.

## Non-goals

Refunds.

## Affected services and packages

checkout

## Acceptance criteria

1. A repeated request with the same key returns the first response.
2. A key is scoped to one account.
3. A key expires after 24 hours.

## Risks

None known.

## Open questions

None.
`;

step("request-edit-refused", async () => {
  // The server validates a spec's structure: a save it refuses keeps the
  // operator's text and shows the server's reason.
  await visit("/requests/req-spec-review");
  await main().getByRole("button", { name: "Edit", exact: true }).click();
  const editor = main().getByRole("textbox", { name: "Edit spec.md" });
  // The checklist beside the text follows it as it is typed, and never blocks Save.
  const checklist = main().getByRole("region", { name: "Structure checklist" });
  await checklist.getByText("6 to fix.", { exact: false }).waitFor();
  await editor.fill("just one line\n");
  await checklist.getByText("9 to fix.", { exact: false }).waitFor();
  check(
    (await checklist.locator('li[data-ok="false"]').first().innerText()).includes("# Spec"),
    "the checklist does not name the first missing heading",
  );
  allowed = [/^422 PUT \/requests\/req-spec-review\/spec$/];
  await main().getByRole("button", { name: "Save", exact: true }).click();
  await page.getByText(/Could not save: .*missing required heading/).waitFor();
  check((await editor.inputValue()) === "just one line\n", "the refused text was lost");
  await main().getByRole("button", { name: "Cancel", exact: true }).click();
  // Unsaved text is never dropped without asking.
  await confirmDialog("Discard changes");
  await page.getByText("Acceptance criteria (2)").waitFor();
});

step("request-edit-spec", async () => {
  await visit("/requests/req-spec-review");
  await main().getByRole("button", { name: "Edit", exact: true }).click();
  await main().getByRole("textbox", { name: "Edit spec.md" }).fill(validSpec);
  // The criteria list writes the same text box: a criterion added and taken
  // out again through it leaves exactly the text that was typed.
  const criteria = main().getByRole("region", { name: "Acceptance criteria" });
  await criteria.getByRole("textbox", { name: "New criterion" }).fill("A key is logged.");
  await criteria.getByRole("button", { name: "Add", exact: true }).click();
  await criteria.getByRole("textbox", { name: "Criterion 4" }).waitFor();
  check(
    (await main().getByRole("textbox", { name: "Edit spec.md" }).inputValue()).includes(
      "3. A key expires after 24 hours.\n4. A key is logged.\n",
    ),
    "the added criterion is not in the text that Save sends",
  );
  await main().getByRole("region", { name: "Your changes" }).waitFor();
  await criteria.getByRole("button", { name: "Remove criterion 4" }).click();
  check(
    (await main().getByRole("textbox", { name: "Edit spec.md" }).inputValue()) === validSpec,
    "adding and removing a criterion did not give the text back byte for byte",
  );
  // A text the checklist calls complete is one the server then accepts.
  await main()
    .getByRole("region", { name: "Structure checklist" })
    .getByText("Structure is complete.", { exact: false })
    .waitFor();
  check(
    await main().getByRole("button", { name: "Approve", exact: true }).isDisabled(),
    "Approve stayed enabled with unsaved edits",
  );
  await main().getByRole("button", { name: "Save", exact: true }).click();
  // The open editor's own list carries the same count: wait for it to close.
  await main().getByRole("textbox", { name: "Edit spec.md" }).waitFor({ state: "detached" });
  await page.getByText("Acceptance criteria (3)").waitFor();
  check(
    (await request("req-spec-review")).spec === validSpec,
    "the server's spec is not the text that was saved",
  );
  // The save is on the request's own record: who edited which file, with the
  // text it replaced kept as a revision, and the page's audit lists it.
  // The name is the one this browser has stored, or the server's own
  // principal when the session has not been asked for one yet.
  const editor =
    (await page.evaluate(() => window.localStorage.getItem("factoryOperatorName"))) || "api";
  const edit = (await request("req-spec-review")).edits?.at(-1);
  check(
    edit?.path === "spec.md" && edit.by === editor && edit.diff !== "",
    `the save was not recorded as an edit: ${JSON.stringify(edit)}`,
  );
  const replaced = (await api(`/requests/req-spec-review/revisions/${edit.revision}`)).body;
  check(replaced.kind === "edit", "the edit's revision is not marked as an edit");
  const audit = main().getByRole("region", { name: "Audit" });
  await audit.getByRole("button", { name: /Edits in place \(1\)/ }).click();
  await audit.getByText(`Edited by ${editor}`, { exact: false }).waitFor();
  check(
    await main().getByRole("button", { name: "Approve", exact: true }).isEnabled(),
    "Approve stayed disabled after the save",
  );
});

step("request-approve-spec", async () => {
  await visit("/requests/req-spec-review");
  const shown = await request("req-spec-review");
  await main().getByRole("button", { name: "Approve", exact: true }).click();
  await dialog().getByText("Spec review → Planning").waitFor();
  await confirmDialog("Approve");
  const after = await request("req-spec-review");
  check(after.state === "planning", `state is ${after.state}, want planning`);
  check(after.approved_by === "walk-operator", `approved_by is ${after.approved_by}`);
  check(
    after.approved_sha256["spec.md"] === sha256(shown.spec),
    "the approval is not bound to the spec that was shown",
  );
  // The page moves on without a reload.
  await page.getByText("Planning").first().waitFor();
  check(
    (await main().getByRole("button", { name: "Approve", exact: true }).count()) === 0,
    "Approve is still offered after the approval",
  );
});

step("request-changes", async () => {
  await visit("/requests/req-spec-review-b");
  await main().getByRole("button", { name: "Request changes", exact: true }).click();
  const confirm = dialog().getByRole("button", { name: "Request changes", exact: true });
  check(await confirm.isDisabled(), "a rejection with no reason can be sent");
  // A note tied to the criterion itself, beside the free reason: the server
  // records the place, and the reason every reader sees names it.
  await dialog()
    .getByLabel("Place", { exact: true })
    .selectOption({ label: "spec.md · Acceptance criteria · 2. A key is scoped to one account." });
  await dialog().getByLabel("Note on this place").fill("does not say which account");
  await dialog().getByRole("button", { name: "Add note", exact: true }).click();
  await dialog().getByRole("list", { name: "Anchored notes" }).waitFor();
  await dialog().getByLabel("Reason").fill("Otherwise fine.");
  await confirmDialog("Request changes");
  const after = await request("req-spec-review-b");
  check(after.state !== "spec_review", "the request stayed in spec_review");
  check(
    after.rejections?.at(-1)?.reason ===
      "- spec.md, ## Acceptance criteria, number 2: does not say which account\n\nOtherwise fine.",
    `the reason was not recorded as composed: ${after.rejections?.at(-1)?.reason}`,
  );
  check(
    after.rejections.at(-1).anchors?.[0]?.item === 2,
    "the rejection does not carry its anchor",
  );
  check(after.rejections.at(-1).by === "walk-operator", "the rejection has no operator");
});

step("request-oracle-approve", async () => {
  await visit("/requests/req-oracle-review");
  const listing = (await api("/requests/req-oracle-review/oracle")).body;
  check(listing.files.length === 3, `the oracle has ${listing.files.length} files, want 3`);
  // Every file has to be opened before the approval is offered.
  const approve = main().getByRole("button", { name: "Approve", exact: true });
  check(await approve.isDisabled(), "Approve is enabled before any oracle file was shown");
  for (const file of listing.files) {
    await main()
      .getByRole("button", { name: new RegExp(`^${file.name.replace(".", "\\.")}`) })
      .click();
  }
  await page.getByText("func TestOracleIdempotency").waitFor();
  await approve.click();
  await confirmDialog("Approve");
  const after = await request("req-oracle-review");
  check(after.state === "planning", `state is ${after.state}, want planning`);
  for (const file of listing.files) {
    check(
      after.approved_sha256[`oracle/${file.name}`] === file.sha256,
      `oracle/${file.name} is not pinned to the hash that was shown`,
    );
  }
});

step("request-plan-approve", async () => {
  await visit("/requests/req-plan-review");
  const shown = await request("req-plan-review");
  const listing = (await api("/requests/req-plan-review/tickets/1/oracle")).body;
  const approve = main().getByRole("button", { name: "Approve", exact: true });
  check(await approve.isDisabled(), "the plan can be approved before its oracle files were shown");
  // This plan was rejected once: the re-review opens on what changed, with
  // the note against the section it was written on and whether the drafter
  // was given it, read from the server's own revision record.
  const revision = (await api("/requests/req-plan-review/revisions")).body.at(-1);
  check(revision.feedback_supplied === true, "the seeded revision records no hand-off");
  const changes = main().getByRole("region", { name: "Changes since you rejected" });
  await changes.getByText("The drafter was given this note for the redraft.").waitFor();
  const noted = changes
    .getByRole("list", { name: "Your notes on specific places" })
    .getByRole("listitem");
  await noted.getByText("tickets/001.spec.md · Steps").waitFor();
  await noted.getByText("- 1. migrate and switch over in one step").waitFor();
  await noted.getByText("+ 1. s", { exact: true }).waitFor();
  // The spec is not on the page at plan review: the coverage view is where
  // a ticket's "- 1" meets the criterion's text.
  const coverage = main().getByRole("region", { name: "Criteria coverage" });
  await coverage.getByRole("status").waitFor();
  check(
    (await coverage.getByRole("rowheader").count()) > 0,
    "the coverage view lists no acceptance criterion",
  );
  for (const file of listing.files) {
    await main()
      .getByRole("button", { name: new RegExp(`^${file.name.replace(".", "\\.")}`) })
      .click();
  }
  await page.getByText(`${listing.files.length} of ${listing.files.length} shown`).waitFor();
  await approve.click();
  await dialog().getByText("Plan review → Building").waitFor();
  await confirmDialog("Approve");
  const after = await request("req-plan-review");
  check(after.state === "building", `state is ${after.state}, want building`);
  for (const ticket of shown.tickets) {
    const key = `tickets/${String(ticket.index).padStart(3, "0")}.spec.md`;
    check(
      after.approved_sha256[key] === sha256(ticket.content),
      `${key} is not bound to the plan that was shown`,
    );
  }
  for (const file of listing.files) {
    check(
      after.approved_sha256[`tickets/001.oracle/${file.name}`] === file.sha256,
      `tickets/001.oracle/${file.name} is not pinned`,
    );
  }
});

step("request-building", async () => {
  await visit("/requests/req-building");
  const pr = main().getByRole("link", { name: "https://github.com/acme/app/pull/7" });
  check(
    (await pr.getAttribute("target")) === "_blank",
    "the pull request link does not open a new tab",
  );
  check(
    (await pr.getAttribute("rel"))?.includes("noopener"),
    "the pull request link has no rel=noopener",
  );
  await main().getByRole("link", { name: "View run" }).first().click();
  await page.waitForURL(/\/runs\/run-/);
});

step("request-send-back", async () => {
  await visit("/requests/req-quarantined");
  await page
    .getByText(/verify failed after 3 rounds/)
    .first()
    .waitFor();
  await main().getByRole("button", { name: "Send back to planning", exact: true }).click();
  await dialog().getByLabel("Reason").fill("The plan misses the account scoping.");
  await nameIfAsked();
  await dialog()
    .getByRole("button", { name: /^Send back/ })
    .click();
  await dialog().waitFor({ state: "detached" });
  const after = await request("req-quarantined");
  check(after.state !== "quarantined", "the request stayed quarantined");
  check(
    after.rejections?.at(-1)?.reason === "The plan misses the account scoping.",
    "the send-back reason was not recorded",
  );
});

step("request-override-link", async () => {
  await visit("/requests/req-halted");
  await page
    .getByText(/its pull request could not be opened/)
    .first()
    .waitFor();
  await visit("/runs/run-quarantined?reason=Request%20req-quarantined%20ticket%201%20quarantined");
  // No override token on this server: the action explains itself, disabled.
  check(
    await button("Override run").isDisabled(),
    "Override run is enabled with no override token",
  );
});

step("request-retry", async () => {
  await visit("/requests/req-halted");
  await main()
    .getByRole("button", { name: /^Retry request/ })
    .click();
  await dialog().getByLabel("Reason").fill("gh is logged in again.");
  await nameIfAsked();
  allowed = [/^(4|5)\d\d POST \/requests\/req-halted\/retry$/];
  const answered = page.waitForResponse((r) => r.url().endsWith("/requests/req-halted/retry"));
  await dialog()
    .getByRole("button", { name: /^Retry/ })
    .click();
  const response = await answered;
  if (response.ok()) {
    await dialog().waitFor({ state: "detached" });
    check(
      (await request("req-halted")).state !== "halted",
      "the request stayed halted after an accepted retry",
    );
  } else {
    // No forge behind this server: the refusal must be on screen, in the dialog.
    await dialog().getByRole("alert").waitFor();
    console.log(
      `      (retry refused by the server with ${response.status()}; its message is shown in the dialog)`,
    );
    await dialog().getByRole("button", { name: "Cancel", exact: true }).click();
  }
});

step("request-cancel", async () => {
  await visit("/requests/req-halted-b");
  await main().getByRole("button", { name: "Cancel request", exact: true }).click();
  await dialog().getByLabel("Reason").fill("No longer needed.");
  await nameIfAsked();
  await dialog()
    .getByRole("button", { name: /^Cancel request/ })
    .click();
  await dialog().waitFor({ state: "detached" });
  const after = await request("req-halted-b");
  check(after.state === "cancelled", `state is ${after.state}, want cancelled`);
  await page.getByText("Cancelled").first().waitFor();
});

step("request-resume", async () => {
  await visit("/requests/req-every-field");
  await main().getByRole("button", { name: "Rebuild from scratch", exact: true }).click();
  await nameIfAsked();
  allowed = [/^(4|5)\d\d POST \/requests\/req-every-field\/resume$/];
  const answered = page.waitForResponse((r) =>
    r.url().endsWith("/requests/req-every-field/resume"),
  );
  await dialog().getByRole("button").last().click();
  const response = await answered;
  if (response.ok()) {
    await dialog().waitFor({ state: "detached" });
    check(
      (await request("req-every-field")).state !== "resume_review",
      "the request stayed in resume_review",
    );
  } else {
    await dialog().getByRole("alert").waitFor();
    console.log(
      `      (resume refused by the server with ${response.status()}; its message is shown in the dialog)`,
    );
  }
});

step("request-revisions", async () => {
  await visit("/requests/req-spec-review-b");
  // The rejection made earlier in this walk left a revision to compare with.
  const revisions = (await api("/requests/req-spec-review-b/revisions")).body;
  check(revisions.length >= 1, "the rejection left no revision");
  await main()
    .getByRole("button", { name: /Rejection history/ })
    .click();
  await page.getByText("does not say which account", { exact: false }).first().waitFor();
});

step("triage-approve", async () => {
  await visit("/triage");
  const left = (await api("/requests")).body.filter(
    (r) => r.state === "spec_review" || r.state === "plan_review",
  );
  if (left.length === 0) {
    await page
      .getByText(/Nothing|No requests|all clear/i)
      .first()
      .waitFor();
    return;
  }
  await page.keyboard.press("a");
  await dialog().getByText("Approve this request?").waitFor();
  await dialog().getByRole("button", { name: "Cancel", exact: true }).click();
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
    const open = await dialog()
      .first()
      .innerText()
      .catch(() => "");
    console.log(
      `      at ${page.url()}\n      controls: ${controls.join(" | ")}${open ? `\n      dialog: ${open.replace(/\n+/g, " / ").slice(0, 400)}` : ""}`,
    );
    await page.keyboard.press("Escape").catch(() => undefined);
  }
}

await browser.close();
const failed = results.filter((result) => result.failure);
console.log(
  `\n${results.length - failed.length} of ${results.length} steps passed. Screenshots: ${shots}`,
);
process.exit(failed.length === 0 ? 0 : 1);
