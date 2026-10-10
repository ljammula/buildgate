// layout.mjs <base-url> <start-token> <walk-dir>
//
// Measures the board in the dark theme against the bars below and exits 1 when
// one is missed. Run it on the dense seed (WALK_SEED=dense), which has the long
// titles and the second project the bars are about. Cards and column headers
// are found by the test ids the board already has; nothing is added to the app.
//
//   a  1440x900  top of the column headers <= 240, top of the first card <= 300
//   b  1280x800  fewest characters of a title (of 40 or more) the card shows >= 28
//   c  1280x800, 960x900 and 700x900, on /, /runs, /triage and
//      /requests/req-plan-review: elements that overflow sideways without
//      being a scroll container or a deliberate truncation: 0
import { chromium } from "playwright";

const [base, token] = process.argv.slice(2);
const channel = process.env.WALK_BROWSER_CHANNEL;
const browser = await chromium.launch({ ...(channel ? { channel } : {}), headless: true });
const context = await browser.newContext({
  viewport: { width: 1440, height: 900 },
  colorScheme: "dark",
});
const page = await context.newPage();
await page.goto(`${base}/#t=${token}`);
await page.getByRole("navigation", { name: "Main" }).waitFor();

async function openBoard(width, height) {
  await page.setViewportSize({ width, height });
  await page.goto(`${base}/`);
  await page.locator('[data-testid^="card-"]').first().waitFor({ timeout: 10000 });
  // Widths are measured: wait for the fonts they depend on.
  await page.evaluate(() => document.fonts.ready);
  await page.waitForTimeout(600);
}

const results = [];
function report(id, value, bar, pass) {
  results.push(pass);
  console.log(`${pass ? "PASS" : "FAIL"} ${id}: ${value} (bar ${bar})`);
}

// a: how far down the board starts.
await openBoard(1440, 900);
const tops = await page.evaluate(() => {
  const top = (selector) =>
    Math.min(...[...document.querySelectorAll(selector)].map((e) => e.getBoundingClientRect().top));
  return { headers: top('[data-testid^="column-"]'), card: top('[data-testid^="card-"]') };
});
report("column-headers-top@1440x900", Math.round(tops.headers), "<= 240", tops.headers <= 240);
report("first-card-top@1440x900", Math.round(tops.card), "<= 300", tops.card <= 300);

// b: how much of a long title the card shows. The longest prefix whose text
// still lies inside the link's box, found with a Range, counts as shown; the
// ellipsis itself is not counted.
await openBoard(1280, 800);
const visible = await page.evaluate(() => {
  const out = [];
  for (const card of document.querySelectorAll('[data-testid^="card-"]')) {
    const link = card.querySelector("a");
    const text = link?.firstChild;
    if (!link || text?.nodeType !== Node.TEXT_NODE || text.length < 40) continue;
    const box = link.getBoundingClientRect();
    const fits = (n) => {
      const range = document.createRange();
      range.setStart(text, 0);
      range.setEnd(text, n);
      return [...range.getClientRects()].every(
        (r) => r.right <= box.right + 0.5 && r.bottom <= box.bottom + 0.5,
      );
    };
    let lo = 0;
    let hi = text.length;
    while (lo < hi) {
      const mid = Math.ceil((lo + hi) / 2);
      if (fits(mid)) lo = mid;
      else hi = mid - 1;
    }
    out.push({ id: card.dataset.testid, length: text.length, shown: lo });
  }
  return out;
});
const fewest = visible.reduce((a, b) => (b.shown < a.shown ? b : a), visible[0]);
report(
  "shortest-visible-title@1280x800",
  fewest
    ? `${fewest.shown} of ${fewest.length} characters (${fewest.id}), ${visible.length} cards`
    : "no card with a long title",
  ">= 28",
  fewest !== undefined && fewest.shown >= 28,
);

// c: sideways overflow that nothing intends.
const overflowing = () =>
  page.evaluate(() => {
    const found = [];
    for (const e of document.querySelectorAll("body *")) {
      if (e.clientWidth === 0 || e.scrollWidth <= e.clientWidth + 1) continue;
      const style = getComputedStyle(e);
      if (["auto", "scroll"].includes(style.overflowX)) continue;
      if (e.closest(".sr-only")) continue;
      // Screen-reader-only text is absolutely positioned and widens
      // scrollWidth without being drawn: measure the text that is.
      const box = e.getBoundingClientRect();
      const walker = document.createTreeWalker(e, NodeFilter.SHOW_TEXT);
      let right = 0;
      for (let node = walker.nextNode(); node; node = walker.nextNode()) {
        if (node.parentElement?.closest(".sr-only") || !node.textContent?.trim()) continue;
        const range = document.createRange();
        range.selectNodeContents(node);
        right = Math.max(right, range.getBoundingClientRect().right);
      }
      if (right <= box.right + 1) continue;
      if (style.textOverflow === "ellipsis" || /truncate|line-clamp/.test(String(e.className)))
        continue;
      found.push(
        `${e.tagName.toLowerCase()}.${String(e.className).trim().replace(/\s+/g, ".").slice(0, 60)} "${(e.textContent ?? "").trim().slice(0, 30)}"`,
      );
    }
    // The page itself must not scroll sideways, whatever element causes it.
    if (document.documentElement.scrollWidth > innerWidth + 1) {
      found.push(`page scrolls sideways: ${document.documentElement.scrollWidth} > ${innerWidth}`);
    }
    return { found, scanned: document.querySelectorAll("body *").length };
  });
for (const route of ["/", "/runs", "/triage", "/requests/req-plan-review"]) {
  for (const [width, height] of [
    [1280, 800],
    [960, 900],
    [700, 900],
  ]) {
    if (route === "/") await openBoard(width, height);
    else {
      await page.setViewportSize({ width, height });
      await page.goto(`${base}${route}`);
      await page.locator("h1").first().waitFor({ timeout: 10000 });
      await page.evaluate(() => document.fonts.ready);
      await page.waitForTimeout(600);
    }
    const { found, scanned } = await overflowing();
    // A page that did not render has nothing to overflow: that is not a pass.
    report(
      `unintended-overflow ${route}@${width}x${height}`,
      `${found.length} of ${scanned} elements`,
      "0",
      found.length === 0 && scanned > 50,
    );
    for (const line of found.slice(0, 10)) console.log(`  ${line}`);
  }
}

await browser.close();
process.exit(results.every(Boolean) ? 0 : 1);
