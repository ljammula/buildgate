// shots.mjs <base-url> <start-token> <walk-dir> [route...]
//
// Photographs every screen (or the routes given) in the dark and the light
// theme, full page, into <walk-dir>/shots/<theme>-<route>.png, and reports
// any console error or failed request. For looking at the design; the walk
// (walk.mjs) is what checks behaviour.
import fs from "node:fs";
import path from "node:path";

import { chromium } from "playwright";

const [base, token, walkDir, ...only] = process.argv.slice(2);
const shots = path.join(walkDir, "shots");
fs.mkdirSync(shots, { recursive: true });

const routes =
  only.length > 0
    ? only
    : [
        "/",
        "/?group=needs-you",
        "/triage",
        "/runs",
        "/runs/run-accepted",
        "/runs/run-accepted?view=diff",
        "/runs/run-accepted?view=release",
        "/runs/run-quarantined",
        "/runs/run-running",
        "/app/projects",
        "/app/runs/new",
        "/projects/app/stats",
        "/projects/app/release",
        "/ops",
        "/requests/new",
        "/requests/req-spec-review",
        "/requests/req-oracle-review",
        "/requests/req-plan-review",
        "/requests/req-building",
        "/requests/req-quarantined",
        "/requests/req-halted",
        "/requests/req-done",
        "/requests/req-every-field",
      ];

const channel = process.env.WALK_BROWSER_CHANNEL;
const browser = await chromium.launch({ ...(channel ? { channel } : {}), headless: true });
const problems = [];
for (const theme of ["dark", "light"]) {
  const context = await browser.newContext({
    viewport: { width: 1440, height: 900 },
    colorScheme: theme,
  });
  const page = await context.newPage();
  page.on("pageerror", (error) => problems.push(`${page.url()} uncaught: ${error.message}`));
  page.on("response", (response) => {
    if (response.status() >= 400) problems.push(`${response.status()} ${response.url()}`);
  });
  await page.goto(`${base}/#t=${token}`);
  await page.getByRole("navigation", { name: "Main" }).waitFor();
  for (const route of routes) {
    await page.goto(base + route);
    await page
      .locator("h1")
      .first()
      .waitFor({ timeout: 10000 })
      .catch(() => {
        problems.push(`${route}: no h1`);
      });
    await page.waitForTimeout(600);
    const name = route.replace(/[^a-z0-9]+/gi, "_").replace(/^_|_$/g, "") || "board";
    await page.screenshot({ path: path.join(shots, `${theme}-${name}.png`), fullPage: true });
  }
  await context.close();
}
await browser.close();
console.log(`${routes.length} screens in 2 themes: ${shots}`);
console.log(problems.length > 0 ? `problems:\n${problems.join("\n")}` : "no errors");
