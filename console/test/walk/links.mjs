// links.mjs <base-url> <start-token> <walk-dir> [start-path...]
//
// Follows every link of the console the way an operator would: it opens each
// screen it can reach from the start paths (default "/"), selects each tab
// and opens each folded section, and clicks every link it finds there. It
// presses no button that writes, so it may be pointed at a console in use:
//
//   WALK_TOKEN="$(cat ~/.config/factoryd/serve-start-token)" \
//     node test/walk/links.mjs http://127.0.0.1:8090 "" ~/buildgate/link-walk
//
// Without the start token the project stats and release reads answer 403,
// and the walk reports them.
//
// A link inside the console passes when the click lands on a screen that is
// not the unknown-path fallback, with no console error, uncaught exception or
// 4xx/5xx, and when a reload of the address it landed on draws the same
// screen (a path that is also an API read would answer a reload with JSON).
// A link to another origin is not followed: its address must be an absolute
// http(s) URL, and LINKS_CHECK_EXTERNAL=1 also asks it for an answer.
//
// LINKS_PER_SHAPE (default 0, no limit) bounds how many screens of one shape
// (a request page, a run page) are opened for their own links; every link
// already found is still clicked. Exits 1 when a link fails.
//
// Besides links it presses the controls that only change what is drawn (tabs,
// filter chips, view toggles, list rows, folded sections) and treats an
// address one of them leads to as another screen.
import fs from "node:fs";
import path from "node:path";

import { chromium } from "playwright";

const [base, tokenArgument, walkDir, ...starts] = process.argv.slice(2);
// A token that outlives the server (the stable one of an installed serve) is
// passed as WALK_TOKEN, so it is in no process list.
const token = tokenArgument || (process.env.WALK_TOKEN ?? "");
if (!base || walkDir === undefined) {
  console.error("usage: links.mjs <base-url> <start-token> <walk-dir> [start-path...]");
  process.exit(2);
}
const shots = path.join(walkDir, "shots");
fs.mkdirSync(shots, { recursive: true });
const perShape = Number(process.env.LINKS_PER_SHAPE ?? "0");
const checkExternal = process.env.LINKS_CHECK_EXTERNAL === "1";
const origin = new URL(base).origin;

const channel = process.env.WALK_BROWSER_CHANNEL;
const browser = await chromium.launch({ ...(channel ? { channel } : {}), headless: true });
const context = await browser.newContext({ viewport: { width: 1440, height: 900 } });
const page = await context.newPage();
page.setDefaultTimeout(8000);

let problems = [];
// LINKS_ALLOW: regular expressions, one per line, of the "<status> <method> <path>"
// answers and "alert: <text>" lines the data is known to give (a seeded run
// whose evidence is not on disk).
const allowed = (process.env.LINKS_ALLOW ?? "")
  .split("\n")
  .filter((line) => line.trim() !== "")
  .map((line) => new RegExp(line.trim()));
page.on("console", (message) => {
  if (message.type() !== "error") return;
  const text = message.text();
  if (/favicon|Failed to load resource/.test(text)) return; // the response hook names the URL
  problems.push(`console error: ${text}`);
});
page.on("pageerror", (error) => problems.push(`uncaught: ${error.message}`));
page.on("response", (response) => {
  if (response.status() < 400) return;
  const url = new URL(response.url());
  if (url.pathname === "/favicon.ico") return;
  const line = `${response.status()} ${response.request().method()} ${url.pathname}`;
  if (allowed.some((pattern) => pattern.test(line))) return;
  problems.push(`${response.status()} ${response.request().method()} ${url.pathname}${url.search}`);
});

/** Screens whose path is a pattern, so two of them are one shape. */
const shapes = [
  [/^\/requests\/new$/, "/requests/new"],
  [/^\/requests\/[^/]+$/, "/requests/:id"],
  [/^\/runs\/[^/]+$/, "/runs/:id"],
  [/^\/app\/runs\/new$/, "/app/runs/new"],
];
function shapeOf(address) {
  const url = new URL(address, origin);
  const found = shapes.find(([pattern]) => pattern.test(url.pathname));
  const keys = [...new Set(url.searchParams.keys())].sort().join(",");
  return `${found ? found[1] : url.pathname}${keys === "" ? "" : `?${keys}`}`;
}
/** The paths the console's router knows (routes/paths.ts); anything else draws the board. */
const known = [
  /^\/$/,
  /^\/triage$/,
  /^\/runs$/,
  /^\/ops$/,
  /^\/requests\/[^/]+$/,
  /^\/runs\/[^/]+$/,
  /^\/app\/runs\/new$/,
  /^\/app\/projects$/,
  /^\/projects\/[^/]+\/(stats|release|observations)$/,
];

const local = (address) => {
  const url = new URL(address, origin);
  return url.pathname + url.search + url.hash;
};

async function settle() {
  await page.locator("h1").first().waitFor({ timeout: 10000 });
  // Reads in flight draw "Loading"; a link under one is not there yet.
  await page
    .waitForFunction(() => !/\bLoading\b/.test(document.querySelector("main")?.innerText ?? ""), {
      timeout: 10000,
    })
    .catch(() => {});
  await page.waitForTimeout(250);
}

async function open(address) {
  await page.goto(origin + address);
  await settle();
}

/** What the screen says is wrong: its alerts, and the title it drew. */
async function reading() {
  return page.evaluate(() => ({
    title: document.querySelector("h1")?.textContent?.trim() ?? "",
    alerts: [...document.querySelectorAll('[role="alert"]')]
      .map((node) => node.textContent?.trim().replace(/\s+/g, " ").slice(0, 200) ?? "")
      .filter((text) => text !== ""),
    json: document.body.children.length === 1 && document.body.firstElementChild?.tagName === "PRE",
  }));
}

/** An alert that says a read failed; one that states the work's own condition is not a fault. */
const readFailed = (alert) =>
  /Request failed|could not be read|not found|Failed to/i.test(alert) &&
  !allowed.some((pattern) => pattern.test(`alert: ${alert}`));

async function anchors() {
  return page.evaluate(() =>
    [...document.querySelectorAll("a[href]")].map((a) => ({
      href: a.getAttribute("href") ?? "",
      resolved: a.href,
      text: (a.getAttribute("aria-label") ?? a.textContent ?? "").trim().replace(/\s+/g, " "),
      target: a.getAttribute("target") ?? "",
    })),
  );
}

/**
 * Controls that change what is drawn and write nothing to the server: tabs,
 * pressed-state chips and toggles, the selected row of a list, and folded
 * sections. Pressing one either changes the address (another screen to
 * survey) or shows links that were not there.
 */
const controls =
  '[role="tab"], button[aria-pressed], button[aria-current], [data-testid^="triage-row-"], button[aria-expanded="false"]';

async function unfold() {
  for (const summary of await page.locator("details:not([open]) > summary").all())
    await summary.click({ timeout: 2000 }).catch(() => {});
}

/** Every link of the screen at `address`, and the addresses its controls lead to. */
async function survey(address) {
  await open(address);
  const state = await reading();
  const found = new Map();
  const pressed = [];
  const collect = async (reveal) => {
    for (const anchor of await anchors())
      if (!found.has(anchor.href)) found.set(anchor.href, { ...anchor, reveal });
  };
  await collect(null);
  await unfold();
  await collect(null);
  // What went wrong on this screen as it opens: a control pressed below leads
  // to another screen, whose own survey reports what goes wrong there.
  const faults = [...problems];
  const count = await page.locator(controls).count();
  for (let i = 0; i < count; i++) {
    const control = page.locator(controls).nth(i);
    if (!(await control.isVisible().catch(() => false))) continue;
    const name = ((await control.textContent().catch(() => "")) ?? "").trim().slice(0, 40);
    if (
      !(await control.click({ timeout: 3000 }).then(
        () => true,
        () => false,
      ))
    )
      continue;
    await page.waitForTimeout(400);
    const now = local(page.url());
    if (now !== address) pressed.push({ address: now, name });
    else {
      await unfold();
      await collect(i);
    }
    await open(address);
  }
  return { links: [...found.values()], pressed, state, faults };
}

const results = [];
const seenShape = new Map();
const queued = new Set();
const queue = [];
function enqueue(address, from) {
  if (queued.has(address)) return;
  queued.add(address);
  const shape = shapeOf(address);
  const count = seenShape.get(shape) ?? 0;
  if (perShape > 0 && count >= perShape) return;
  seenShape.set(shape, count + 1);
  queue.push({ address, from });
}

async function external(link) {
  let url;
  try {
    url = new URL(link.href);
  } catch {
    return { ok: false, why: "not an absolute URL" };
  }
  if (url.protocol !== "http:" && url.protocol !== "https:")
    return { ok: false, why: `scheme ${url.protocol}` };
  if (!checkExternal) return { ok: true, why: "not followed" };
  try {
    const response = await fetch(link.href, {
      redirect: "follow",
      signal: AbortSignal.timeout(15000),
    });
    // A private page answers 404 to a caller who is not signed in: the address is still right.
    const privateHost = /(^|\.)github\.com$/.test(url.hostname) && response.status === 404;
    return {
      ok: response.status < 400 || privateHost,
      why: `${response.status}${privateHost ? " (signed-out read of a private page)" : ""}`,
    };
  } catch (error) {
    return { ok: false, why: `no answer: ${error.cause?.code ?? error.message}` };
  }
}

/** Click `link` on the screen at `from` and say where it led. */
async function follow(link, from) {
  problems = [];
  await open(from);
  if (link.reveal !== null) {
    await page.locator(controls).nth(link.reveal).click();
    await page.waitForTimeout(400);
  }
  await unfold();
  const anchor = page.locator(`a[href="${link.href.replace(/["\\]/g, "\\$&")}"]`).first();
  if ((await anchor.count()) === 0) return { ok: false, why: "the link is gone on a second look" };
  problems = [];
  let how = "click";
  // Caught here: a click that fails first would leave this rejection unhandled.
  const popup =
    link.target === "_blank"
      ? context.waitForEvent("page", { timeout: 5000 }).catch(() => null)
      : null;
  try {
    await anchor.scrollIntoViewIfNeeded({ timeout: 2000 }).catch(() => {});
    await anchor.click({ timeout: 3000 });
  } catch {
    // Drawn only on hover or focus (a compact card's link): reach it by keyboard.
    how = "focus + Enter";
    await anchor.focus();
    await page.keyboard.press("Enter");
  }
  if (popup !== null) {
    const landed = await popup;
    if (landed === null) return { ok: false, why: "target=_blank opened no tab" };
    await landed.waitForLoadState("load");
    const state = { address: landed.url() };
    await landed.close();
    return { ok: state.address.startsWith(origin), why: `new tab ${state.address}`, how };
  }
  const wanted = local(link.resolved);
  await page
    .waitForURL((url) => local(url.href) !== from || wanted === from, { timeout: 5000 })
    .catch(() => {});
  await settle().catch(() => {});
  const address = local(page.url());
  const state = await reading();
  const faults = [...problems];
  const pathname = new URL(page.url()).pathname;
  if (!known.some((pattern) => pattern.test(pathname))) faults.push(`unknown path ${pathname}`);
  if (state.title === "") faults.push("no heading");
  if (state.json) faults.push("the server answered with JSON");
  if (address === from && wanted !== from && !wanted.startsWith(`${from}#`))
    faults.push("the address did not change");
  for (const alert of state.alerts.filter(readFailed)) faults.push(`alert: ${alert}`);
  // A reload must draw the same screen.
  problems = [];
  await page.reload();
  await settle().catch(() => {});
  const again = await reading();
  if (again.json) faults.push("a reload answered with JSON");
  else if (again.title !== state.title)
    faults.push(`a reload drew "${again.title}" instead of "${state.title}"`);
  for (const problem of problems) faults.push(`on reload: ${problem}`);
  return { ok: faults.length === 0, why: faults.join("; "), how, address, title: state.title };
}

if (token) {
  // A failed load would name its address, and the address carries the token.
  await page.goto(`${origin}/#t=${token}`).catch(() => {
    throw new Error(`${origin} did not answer`);
  });
  await page.getByRole("navigation", { name: "Main" }).waitFor();
}
// A pressed toggle is kept by the browser (the board's List view): every screen
// is opened with the storage the walk started with, so each survey sees the same.
await page.goto(`${origin}/`);
await settle();
const kept = await page.evaluate(() => JSON.stringify(Object.entries(window.localStorage)));
await context.addInitScript((entries) => {
  window.localStorage.clear();
  for (const [key, value] of JSON.parse(entries)) window.localStorage.setItem(key, value);
}, kept);
for (const start of starts.length > 0 ? starts : ["/"]) enqueue(start, "start");

const clicked = new Set();
let screens = 0;
while (queue.length > 0) {
  const { address, from } = queue.shift();
  problems = [];
  let found;
  try {
    found = await survey(address);
  } catch (error) {
    results.push({
      ok: false,
      from,
      href: address,
      text: "(screen)",
      why: `did not open: ${error.message.split("\n")[0]}`,
    });
    continue;
  }
  screens++;
  const faults = [
    ...found.faults,
    ...found.state.alerts.filter(readFailed).map((alert) => `alert: ${alert}`),
  ];
  if (faults.length > 0)
    results.push({
      ok: false,
      from,
      href: address,
      text: "(screen)",
      why: [...new Set(faults)].join("; "),
    });
  for (const control of found.pressed) enqueue(control.address, `${address} [${control.name}]`);
  for (const link of found.links) {
    const isLocal = link.resolved.startsWith(`${origin}/`) || link.resolved === origin;
    const key = isLocal ? local(link.resolved) : link.href;
    if (clicked.has(key)) continue;
    clicked.add(key);
    let outcome;
    try {
      outcome = isLocal ? await follow(link, address) : await external(link);
    } catch (error) {
      outcome = { ok: false, why: `threw: ${error.message.split("\n")[0]}` };
    }
    results.push({
      from: address,
      href: link.href,
      text: link.text,
      kind: isLocal ? "console" : "outside",
      ...outcome,
    });
    if (!outcome.ok)
      await page
        .screenshot({ path: path.join(shots, `link-fail-${results.length}.png`), fullPage: true })
        .catch(() => {});
    if (isLocal && outcome.address !== undefined) enqueue(outcome.address, address);
  }
}
await browser.close();

const failed = results.filter((result) => !result.ok);
fs.writeFileSync(path.join(walkDir, "links.json"), JSON.stringify(results, null, 2));
for (const result of results)
  console.log(
    `${result.ok ? "PASS" : "FAIL"}  ${result.from}  ->  ${result.href}  [${result.text.slice(0, 50)}]${result.why ? `  ${result.why}` : ""}`,
  );
const inside = results.filter((result) => result.kind === "console").length;
console.log(
  `\n${screens} screens, ${inside} console links clicked, ${results.filter((result) => result.kind === "outside").length} outside links read, ${failed.length} failed`,
);
process.exit(failed.length > 0 ? 1 : 0);
