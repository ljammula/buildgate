// The React console, driven the way test/walk/walk.mjs drives it. Every
// function takes the Playwright page of one console and performs one
// operator action; drivers/flutter.mjs exports the same set.
const origin = (page) => new URL(page.url()).origin;
const main = (page) => page.getByRole("main");
const dialog = (page) => page.getByRole("dialog");

/** The operator-name prompt, should the stored name be missing. */
async function nameIfAsked(page) {
  const field = page.getByLabel("Operator name");
  if (await field.isVisible().catch(() => false)) {
    await field.fill("parity-operator");
    await page.getByRole("button", { name: "Continue" }).click();
  }
}

async function confirmDialog(page, name) {
  await nameIfAsked(page);
  await dialog(page).getByRole("button", { name, exact: true }).click();
  await dialog(page).waitFor({ state: "detached" });
}

export async function openRequest(page, id) {
  await page.goto(`${origin(page)}/requests/${id}`);
  await page.locator("h1").first().waitFor();
}

export async function approve(page) {
  await main(page).getByRole("button", { name: "Approve", exact: true }).click();
  await confirmDialog(page, "Approve");
}

export async function requestChanges(page, reason) {
  await main(page).getByRole("button", { name: "Request changes", exact: true }).click();
  await dialog(page).getByLabel("Reason").fill(reason);
  await confirmDialog(page, "Request changes");
}

export async function editSpec(page, text) {
  await main(page).getByRole("button", { name: "Edit", exact: true }).click();
  await main(page).getByRole("textbox", { name: "Edit spec.md" }).fill(text);
  await main(page).getByRole("button", { name: "Save", exact: true }).click();
  // Saved once the editor is gone and Approve is offered again.
  await main(page).getByRole("button", { name: "Approve", exact: true }).waitFor();
  await page.waitForFunction(
    () => ![...document.querySelectorAll("button")].some((b) => b.textContent.trim() === "Save"),
  );
}

export async function openOracleFiles(page, names) {
  for (const name of names) {
    await main(page)
      .getByRole("button", { name: new RegExp(`^${name.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}`) })
      .click();
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
  await main(page).getByRole("button", { name: "Send back to planning", exact: true }).click();
  await dialog(page).getByLabel("Reason").fill(reason);
  await nameIfAsked(page);
  await dialog(page)
    .getByRole("button", { name: /^Send back/ })
    .click();
  await dialog(page).waitFor({ state: "detached" });
}

/** A refusal leaves the dialog open: the caller records the status. */
export async function retry(page, reason) {
  await main(page)
    .getByRole("button", { name: /^Retry request/ })
    .click();
  await dialog(page).getByLabel("Reason").fill(reason);
  await nameIfAsked(page);
  const answered = page.waitForResponse((r) => r.url().endsWith("/retry"));
  await dialog(page)
    .getByRole("button", { name: /^Retry/ })
    .click();
  await answered;
}

export async function cancel(page, reason) {
  await main(page).getByRole("button", { name: "Cancel request", exact: true }).click();
  await dialog(page).getByLabel("Reason").fill(reason);
  await nameIfAsked(page);
  await dialog(page)
    .getByRole("button", { name: /^Cancel request/ })
    .click();
  await dialog(page).waitFor({ state: "detached" });
}

export async function resumeFromScratch(page) {
  await main(page).getByRole("button", { name: "Rebuild from scratch", exact: true }).click();
  await nameIfAsked(page);
  const answered = page.waitForResponse((r) => r.url().endsWith("/resume"));
  await dialog(page).getByRole("button").last().click();
  await answered;
}

export async function newRequest(
  page,
  { workspace, text, verifyCommand, preflightProfile, draftOracles },
) {
  await page.goto(`${origin(page)}/requests/new`);
  await page.getByLabel("Workspace path").fill(workspace);
  await page.getByLabel("Request", { exact: true }).fill(text);
  if (draftOracles) await page.getByLabel("Draft oracles").check();
  await page.getByRole("button", { name: "Advanced" }).click();
  await page.getByLabel(/Verify command/).fill(verifyCommand);
  await page.getByLabel(/Preflight profile/).fill(preflightProfile);
  await page.getByRole("button", { name: "Submit request" }).click();
  await nameIfAsked(page);
  await page.waitForURL(/\/requests\/(?!new$)[^/]+$/);
}
