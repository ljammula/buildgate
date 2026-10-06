// record.mjs <base-url> <start-token> <walk-dir>
//
// Records one continuous tour of the console as a video, against the same
// freshly seeded server the walk uses:
//
//   WALK_SCRIPT=record.mjs console/test/walk/run.sh
//
// The result is <walk-dir>/recording/console-tour.webm (VP8, as Playwright
// writes it; no other tool is needed). Captions are drawn into the page, so
// they are part of the picture. Every path under a home directory is
// rewritten in the page before it is painted (/Users/<name> and /home/<name>
// become /Users/operator), and the tour fails if a home path is still in the
// page's text at any step, so a recording can be published as it is.
//
// RECORD_STILLS=1 also writes still-NN.png beside it, one per caption.
//
// It is a tour, not a check: walk.mjs is what asserts behaviour.
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

import { chromium } from "playwright";

const [base, token, walkDir] = process.argv.slice(2);
const outDir = path.join(walkDir, "recording");
fs.rmSync(outDir, { recursive: true, force: true });
fs.mkdirSync(outDir, { recursive: true });

const size = { width: 1280, height: 800 };
const channel = process.env.WALK_BROWSER_CHANNEL;
const browser = await chromium.launch({ ...(channel ? { channel } : {}), headless: true });
const context = await browser.newContext({
  viewport: size,
  colorScheme: "dark",
  recordVideo: { dir: outDir, size },
});

// Runs in the page before any of its own code: masks home paths in every
// text node as it appears, and owns the caption bar.
await context.addInitScript(() => {
  const home = /\/(Users|home)\/[A-Za-z0-9._-]+(…)?/g;
  const mask = (node) => {
    if (node.nodeType === Node.TEXT_NODE) {
      const text = node.nodeValue ?? "";
      const masked = text.replace(home, "/$1/operator");
      if (masked !== text) node.nodeValue = masked;
      return;
    }
    if (node.nodeType !== Node.ELEMENT_NODE) return;
    for (const attribute of ["title", "aria-label", "value", "placeholder"]) {
      const value = node.getAttribute(attribute);
      if (value !== null && home.test(value)) {
        node.setAttribute(attribute, value.replace(home, "/$1/operator"));
      }
      home.lastIndex = 0;
    }
    for (const child of node.childNodes) mask(child);
  };
  const start = () => {
    mask(document.body);
    new MutationObserver((records) => {
      for (const record of records) {
        if (record.type === "characterData") mask(record.target);
        for (const added of record.addedNodes) mask(added);
      }
    }).observe(document.body, { childList: true, subtree: true, characterData: true });
  };
  if (document.body) start();
  else document.addEventListener("DOMContentLoaded", start);

  window.__caption = (text) => {
    let bar = document.getElementById("tour-caption");
    if (bar === null) {
      bar = document.createElement("div");
      bar.id = "tour-caption";
      bar.setAttribute("aria-hidden", "true");
      bar.style.cssText =
        "position:fixed;left:50%;bottom:20px;transform:translateX(-50%);z-index:2147483647;" +
        "max-width:80%;padding:10px 18px;border-radius:10px;background:rgba(15,15,20,.92);" +
        "color:#fff;font:600 16px/1.35 system-ui,sans-serif;text-align:center;" +
        "box-shadow:0 6px 24px rgba(0,0,0,.45);border:1px solid rgba(255,255,255,.18);pointer-events:none";
      document.body.appendChild(bar);
    }
    bar.textContent = text;
    bar.style.display = text === "" ? "none" : "block";
  };
});

const page = await context.newPage();
page.setDefaultTimeout(15000);
const main = () => page.getByRole("main");
const dialog = () => page.getByRole("dialog");
const pause = (ms = 1400) => page.waitForTimeout(ms);
const userName = os.userInfo().username;
// RECORD_STILLS=1 also saves a still per caption, for checking a recording without playing it.
const stills = process.env.RECORD_STILLS === "1";
let stillCount = 0;

/** Sets the caption, checks nothing private is on the page, and lets the viewer read. */
async function say(text, ms = 2600) {
  await page.evaluate((t) => window.__caption(t), text);
  const visible = await page.evaluate(() => document.body.innerText);
  if (visible.includes(os.homedir()) || new RegExp(`/(Users|home)/${userName}`).test(visible)) {
    throw new Error(`a home path is visible at: ${text}`);
  }
  if (stills) {
    await page.screenshot({
      path: path.join(outDir, `still-${String(++stillCount).padStart(2, "0")}.png`),
    });
  }
  await pause(ms);
}
async function visit(pathname) {
  await page.goto(base + pathname);
  await page.locator("h1").first().waitFor();
  await pause(700);
}
async function type(locator, text) {
  await locator.click();
  await locator.pressSequentially(text, { delay: 28 });
}
async function nameIfAsked() {
  const field = page.getByLabel("Operator name");
  if (await field.isVisible().catch(() => false)) {
    await type(field, "operator");
    await pause(500);
    await page.getByRole("button", { name: "Continue" }).click();
  }
}
async function scrollTo(locator) {
  await locator.scrollIntoViewIfNeeded();
  await pause(600);
}

// ------------------------------------------------------------------ tour

await page.goto(`${base}/#t=${token}`);
await page.getByRole("navigation", { name: "Main" }).waitFor();
await say("Buildgate console: every request, grouped by who has to act", 3200);
await say("Needs you: reviews, halted and quarantined requests");

await page.getByRole("link", { name: /^Triage/ }).click();
await page.locator("h1").first().waitFor();
await say("Triage: what waits on you, oldest first, with the reason");

// Spec review: the document, the checklist, the criteria list.
await visit("/requests/req-spec-review");
await say("Spec review: the approval is bound to exactly this text", 3000);
await main().getByRole("button", { name: "Edit", exact: true }).click();
await say("Edit: a checklist shows what the server's validation will say", 3200);
const editor = main().getByRole("textbox", { name: "Edit spec.md" });
await editor.fill(
  [
    "# Spec",
    "",
    "## Problem",
    "",
    "A retried checkout charges the card twice.",
    "",
    "## Scope",
    "",
    "The checkout endpoint.",
    "",
    "## Non-goals",
    "",
    "Refunds.",
    "",
    "## Affected services and packages",
    "",
    "checkout",
    "",
    "## Acceptance criteria",
    "",
    "1. A repeated request with the same key returns the first response.",
    "2. A key is scoped to one account.",
    "",
    "## Risks",
    "",
    "None known.",
    "",
    "## Open questions",
    "",
    "None.",
    "",
  ].join("\n"),
);
await say("With every required section present, the structure is complete", 3000);
const criteria = main().getByRole("region", { name: "Acceptance criteria" });
await scrollTo(criteria);
await type(
  criteria.getByRole("textbox", { name: "New criterion" }),
  "A key expires after 24 hours.",
);
await criteria.getByRole("button", { name: "Add", exact: true }).click();
await say("Criteria are a list: add, edit, reorder; the numbers stay in step", 3200);
await scrollTo(main().getByRole("region", { name: "Your changes" }));
await say("Before saving: a line diff of everything that will be sent", 3200);
await main().getByRole("button", { name: "Save", exact: true }).click();
await nameIfAsked();
await editor.waitFor({ state: "detached" });
await say("Saved through the same validation and hash check as any edit");

// Request changes with a note tied to a criterion.
await visit("/requests/req-spec-review-b");
await main().getByRole("button", { name: "Request changes", exact: true }).click();
await nameIfAsked();
await dialog().getByLabel("Place", { exact: true }).waitFor();
await say("Request changes: tie a note to a section or a criterion", 2800);
await dialog()
  .getByLabel("Place", { exact: true })
  .selectOption({ label: "spec.md · Acceptance criteria · 2. A key is scoped to one account." });
await type(dialog().getByLabel("Note on this place"), "does not say which account");
await dialog().getByRole("button", { name: "Add note", exact: true }).click();
await say("The drafter is told the exact item, not a sentence to locate", 3000);
await dialog().getByRole("button", { name: "Request changes", exact: true }).click();
await dialog().waitFor({ state: "detached" });
await say("Sent back for a redraft, with the note on record");

// Plan review.
await visit("/requests/req-plan-review");
await say("Plan review: which ticket claims each acceptance criterion", 3200);
await scrollTo(main().getByRole("region", { name: "Criteria coverage" }));
await say("A criterion no ticket claims is called out", 3000);
await page.evaluate(() => window.scrollTo({ top: 0, behavior: "smooth" }));
await say("Approve stays off until every oracle file has been opened", 3000);
for (const tile of await main()
  .getByRole("button", { name: /bytes · sha256/ })
  .all()) {
  await tile.click();
  await pause(900);
}
await say("Each file is shown whole; the approval pins them by hash", 3000);
await main().getByRole("button", { name: "Approve", exact: true }).click();
await nameIfAsked();
await dialog().getByText("Approve this request?").waitFor();
await say("The dialog names what is approved and what happens next", 3000);
await dialog().getByRole("button", { name: "Approve", exact: true }).click();
await dialog().waitFor({ state: "detached" });
await say("Approved: the tickets queue to build");

// A build and its run.
await visit("/requests/req-building");
await say("Building: each ticket's run, its stage and last activity", 3000);
await visit("/runs/run-running");
await say("The run page: each stage, and a warning when a build goes quiet", 3200);
await visit("/runs/run-accepted");
await say("A finished run: gates, attempts and evidence", 3000);
await visit("/runs/run-accepted?view=diff");
await say("Its diff", 2600);
await visit("/runs/run-accepted?view=release");
await say("The release decision is recorded evidence only: no merge, no deploy", 3400);

// When something stops.
await visit("/requests/req-quarantined");
await say("Quarantined: the cause, and the ways on: retry, send back, cancel", 3600);
await visit("/requests/req-halted");
await say("Halted: what stopped it and the command that resumes it", 3200);

await visit("/ops");
await say("Operations: queue, daemon and kill-switch state per project", 3000);
await page.getByRole("button", { name: /^Theme:/ }).click();
await pause(500);
await page.getByRole("button", { name: /^Theme:/ }).click();
await visit("/");
await say("Light or dark; merge and deploy always stay with a person", 3400);
await say("", 600);

const video = page.video();
await context.close();
await browser.close();
const target = path.join(outDir, "console-tour.webm");
fs.renameSync(await video.path(), target);
const megabytes = (fs.statSync(target).size / (1024 * 1024)).toFixed(1);
console.log(`recorded ${target} (${megabytes} MB)`);
