// phone.mjs <base-url> <start-token> <walk-dir> [route...]
//
// Opens every screen (or the routes given) at a phone's size and measures it
// against the bars below; photographs go to <walk-dir>/shots/phone-<route>.png.
// Exits 1 when a screen misses a bar.
//
//   - the page is no wider than the screen (nothing to scroll sideways but a
//     box that scrolls on its own: a table, the board, a code block);
//   - nothing in the screen's own content is cut off past the right edge
//     outside such a box;
//   - the navigation takes at most a quarter of the screen's height;
//   - the screen's title is drawn, at least 40px wide, in the first screenful.
import fs from "node:fs";
import path from "node:path";

import { chromium, devices } from "playwright";

const [base, token, walkDir, ...only] = process.argv.slice(2);
if (!base || walkDir === undefined) {
  console.error("usage: phone.mjs <base-url> <start-token> <walk-dir> [route...]");
  process.exit(2);
}
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
        "/app/projects",
        "/app/projects?project=app&tab=stats",
        "/app/projects?project=app&tab=release",
        "/app/runs/new",
        "/requests/new",
        "/requests/req-spec-review",
        "/requests/req-oracle-review",
        "/requests/req-plan-review",
        "/requests/req-building",
        "/requests/req-quarantined",
        "/requests/req-every-field",
      ];

const channel = process.env.WALK_BROWSER_CHANNEL;
const browser = await chromium.launch({ ...(channel ? { channel } : {}), headless: true });
const context = await browser.newContext({ ...devices["iPhone 13"] });
const page = await context.newPage();
if (token) {
  // A failed load would name its address, and the address carries the token.
  await page.goto(`${base}/#t=${token}`).catch(() => {
    throw new Error(`${base} did not answer`);
  });
  await page.getByRole("navigation", { name: "Main" }).waitFor();
}
// The phone's own size: on a page that overflows, the browser's innerWidth
// and innerHeight grow with the content and would hide the overflow.
const screen = page.viewportSize();

let missed = 0;
for (const route of routes) {
  await page.goto(base + route);
  await page.locator("h1").first().waitFor({ state: "attached", timeout: 10000 });
  await page.waitForTimeout(800);
  const facts = await page.evaluate((width) => {
    const boxed = (node) => {
      for (let e = node.parentElement; e !== null; e = e.parentElement) {
        const style = window.getComputedStyle(e);
        if (style.overflowX !== "visible" || style.position === "fixed") return true;
      }
      return false;
    };
    const drawn = (node) => {
      const style = window.getComputedStyle(node);
      // Text kept for a screen reader only is a 1px clipped box, wherever it lies.
      return style.visibility !== "hidden" && style.clip === "auto" && style.clipPath === "none";
    };
    const cut = [...document.querySelectorAll("main *")].filter((node) => {
      const box = node.getBoundingClientRect();
      return box.width > 1 && box.right > width + 1 && drawn(node) && !boxed(node);
    });
    const title = document.querySelector("h1").getBoundingClientRect();
    return {
      page: document.documentElement.scrollWidth,
      navigation: Math.round(document.querySelector("aside").getBoundingClientRect().height),
      title: { width: Math.round(title.width), top: Math.round(title.top) },
      cut: cut.slice(0, 3).map((node) => {
        const name = typeof node.className === "string" ? node.className : "";
        return `<${node.tagName.toLowerCase()} class="${name.slice(0, 60)}"> ${(node.textContent ?? "").trim().slice(0, 30)}`;
      }),
    };
  }, screen.width);
  const misses = [];
  if (facts.page > screen.width + 1)
    misses.push(`the page is ${facts.page}px wide on a ${screen.width}px screen`);
  if (facts.cut.length > 0) misses.push(`cut off at the right edge: ${facts.cut.join(" | ")}`);
  if (facts.navigation > screen.height / 4)
    misses.push(`the navigation is ${facts.navigation}px of a ${screen.height}px screen`);
  if (facts.title.width < 40 || facts.title.top > screen.height)
    misses.push(`the title is ${facts.title.width}px wide at ${facts.title.top}px`);
  const name = route.replace(/[^a-z0-9]+/gi, "_").replace(/^_|_$/g, "") || "board";
  await page.screenshot({ path: path.join(shots, `phone-${name}.png`), fullPage: true });
  console.log(
    `${misses.length === 0 ? "PASS" : "FAIL"}  ${route}${misses.length === 0 ? "" : `  ${misses.join("; ")}`}`,
  );
  if (misses.length > 0) missed++;
}
await browser.close();
console.log(
  `\n${routes.length - missed} of ${routes.length} screens fit a phone. Photographs: ${shots}`,
);
process.exit(missed > 0 ? 1 : 0);
