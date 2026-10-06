// The Flutter console, driven through its accessibility tree (it paints to a
// canvas). Everything particular to Flutter lives here and nowhere else:
//   - semantics must be switched on (flt-semantics-placeholder) after every
//     page load;
//   - only on-screen widgets are in the tree, so a control is found by
//     scrolling until its locator exists (`reveal`);
//   - a dialog's buttons are the last nodes in the tree, so the last
//     same-named button is the dialog's own (the confirm sheet's second
//     "Approve", the dialog's "Request changes" and "Cancel request").
// Exports the same functions as drivers/react.mjs.
const MIN = 60_000;
const origin = (page) => new URL(page.url()).origin;
const escape = (text) => text.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");

async function enableSemantics(page) {
  await page.evaluate(() => {
    const placeholder = document.querySelector("flt-semantics-placeholder");
    if (placeholder) placeholder.click();
  });
  await page.waitForTimeout(500);
}

/** Load a path of the console and turn its accessibility tree on. */
async function load(page, pathname) {
  await page.goto(origin(page) + pathname, { waitUntil: "load" });
  await page.waitForTimeout(3500);
  await enableSemantics(page);
}

const screenText = (page) =>
  page.evaluate(() =>
    [...document.querySelectorAll("flt-semantics")]
      .map((e) => (e.getAttribute("aria-label") || "") + " " + (e.textContent || ""))
      .join("\n"),
  );

async function scrollPage(page, dy) {
  await page.mouse.move(6, 500);
  await page.mouse.wheel(0, dy);
  await page.waitForTimeout(400);
}

/** Scroll (from the left gutter, never inside an editor) until the locator exists. */
async function reveal(page, locator, timeout = 30_000) {
  const end = Date.now() + timeout;
  for (let dir = 1; ; dir = -dir) {
    for (let i = 0; i < 8; i++) {
      if (await locator.count()) return locator.first();
      await scrollPage(page, dir * 600);
    }
    if (Date.now() > end) throw new Error(`timed out revealing ${locator}`);
    await page.waitForTimeout(1000);
  }
}

const button = (page, name) => reveal(page, page.getByRole("button", { name, exact: true }));

/** A dialog's own button: the last one with that name in the tree. */
const dialogButton = (page, name) => page.getByRole("button", { name, exact: true }).last();

async function waitForText(page, re, timeout = 30_000) {
  const end = Date.now() + timeout;
  for (;;) {
    if (re.test(await screenText(page))) return;
    await scrollPage(page, -20000);
    for (let i = 0; i < 12; i++) {
      if (re.test(await screenText(page))) return;
      await scrollPage(page, 700);
    }
    if (Date.now() > end) throw new Error(`timed out waiting for ${re} on screen`);
  }
}

/** The ids are not always on screen: wait for the page's first actions. */
export async function openRequest(page, id) {
  await load(page, `/requests/${id}`);
  await waitForText(
    page,
    /Back to board|Spec review|Plan review|Oracle review|Halted|Quarantined|Resume|Building|Request/,
  );
  await scrollPage(page, -20000);
}

export async function approve(page) {
  await (await button(page, "Approve")).click();
  await page.waitForTimeout(800);
  await dialogButton(page, "Approve").click();
  await page.waitForTimeout(1500);
}

export async function requestChanges(page, reason) {
  await (await button(page, "Request changes")).click();
  await fillField(page, page.getByRole("textbox", { name: "Reason" }), reason);
  await dialogButton(page, "Request changes").click();
  await page.waitForTimeout(1500);
}

export async function editSpec(page, text) {
  await (await button(page, "Edit")).click();
  await page.waitForTimeout(1000);
  const box = page.getByRole("textbox").last();
  await box.click();
  await page.keyboard.press("ControlOrMeta+A");
  await page.keyboard.type(text, { delay: 0 });
  await page.waitForTimeout(500);
  await scrollPage(page, 20000);
  await (await button(page, "Save")).click();
  await page.waitForTimeout(2000);
}

export async function openOracleFiles(page, names) {
  for (const name of names) {
    const tile = await reveal(page, page.getByRole("button", { name: new RegExp(escape(name)) }));
    await tile.click();
    await page.waitForTimeout(1500);
  }
}

export async function approveOracle(page) {
  await approve(page);
}

export async function approvePlan(page, oracleFileNames) {
  await openOracleFiles(page, oracleFileNames);
  await approve(page);
}

export async function sendBack(page, reason) {
  await (await button(page, "Send back to planning")).click();
  await fillField(page, page.getByRole("textbox", { name: "Reason" }), reason);
  await dialogButton(page, "Send back").click();
  await page.waitForTimeout(1500);
}

export async function retry(page, reason) {
  await (await reveal(page, page.getByRole("button", { name: /^Retry request/ }))).click();
  await fillField(page, page.getByRole("textbox", { name: "Reason" }), reason);
  await dialogButton(page, "Retry").click();
  await page.waitForTimeout(2000);
}

export async function cancel(page, reason) {
  await (await button(page, "Cancel request")).click();
  await fillField(page, page.getByRole("textbox", { name: "Reason" }), reason);
  await dialogButton(page, "Cancel request").click();
  await page.waitForTimeout(1500);
}

export async function resumeFromScratch(page) {
  await (await button(page, "Rebuild from scratch")).click();
  await page.waitForTimeout(800);
  await page
    .getByRole("button", { name: /^(Rebuild|Rerun step)$/ })
    .last()
    .click();
  await page.waitForTimeout(2000);
}

/**
 * Playwright's fill() on a Flutter field is unreliable (the value can be lost
 * when focus moves), so click, select all and insert the text as one input.
 */
async function fillField(page, locator, text) {
  await locator.click();
  await page.waitForTimeout(300);
  await page.keyboard.press("ControlOrMeta+A");
  await page.keyboard.insertText(text);
  await page.waitForTimeout(500);
}

export async function newRequest(
  page,
  { workspace, text, verifyCommand, preflightProfile, draftOracles },
) {
  await load(page, "/requests/new");
  await waitForText(page, /Known workspace/);
  await page.waitForTimeout(500);
  await fillField(page, page.getByRole("textbox", { name: "Workspace path" }), workspace);
  await fillField(page, page.getByRole("textbox", { name: "Request" }), text);
  const box = await reveal(page, page.getByRole("checkbox", { name: /Draft oracles/ }));
  if (draftOracles !== (await box.isChecked().catch(() => false))) await box.click();
  await (await reveal(page, page.getByText("Advanced", { exact: true }))).click();
  await page.waitForTimeout(500);
  await fillField(
    page,
    await reveal(page, page.getByRole("textbox", { name: "Verify command" })),
    verifyCommand,
  );
  await fillField(
    page,
    await reveal(page, page.getByRole("textbox", { name: "Preflight profile" })),
    preflightProfile,
  );
  const before = page.url();
  await (await button(page, "Submit request")).click();
  const end = Date.now() + 30_000;
  while (page.url() === before || page.url().endsWith("/requests/new")) {
    if (Date.now() > end) throw new Error(`submit never left the form (${page.url()})`);
    await page.waitForTimeout(500);
  }
}
