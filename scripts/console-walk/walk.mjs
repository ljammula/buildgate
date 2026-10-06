// walk.mjs <base-url> <workspace-path> <scratch-dir>
//
// Drives the embedded factoryd console (the React bundle) the way an operator would, from
// submitting a request through the console's own New request form (no
// `factoryd submit` call at all) to a correctly explained
// terminal state, asserting at each step that the console both let the
// operator act and told them the truth. Rules the walk holds itself to:
//   - it never reloads the page or presses Refresh: every state change must
//     reach the screen on its own;
//   - every operator action is a console click; HTTP GETs below are only
//     assertions about what the server recorded, never actions;
//   - no API token is configured anywhere (loopback single-machine rule).
//
// Steps (each PASS/FAIL, screenshot in <scratch>/shots):
//   submit         New request form (workspace, request.md text, draft
//                  oracles, verify command + preflight profile in
//                  Advanced) submits and lands on the new request's own
//                  /requests/<id> detail page -- the id used by every
//                  later step is derived from that URL, not passed in
//   board          request row visible with a short title, no "queue-run is
//                  not running" strip
//   spec-review    request reaches spec review on screen without a reload
//   reject-note    Request changes with a note; the redraft that comes back
//                  addresses it
//   edit           Edit disables Approve while dirty; Save lands a new
//                  criterion; the parsed-criteria list shows it
//   approve-spec   confirm sheet names the real next state
//   oracle         oracle review approved (all files shown, or skip with
//                  its consequence stated)
//   approve-plan   plan approved
//   build          the ticket's run appears on the request page; the run
//                  page links back to its request and finishes
//   end-state      "Built and verified" callout with a Next that names the
//                  branch and does not contradict itself
//   retry          Retry on the accepted run re-attempts only its pull
//                  request; with no remote the server refuses and the
//                  console shows its reason, the request unchanged
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { chromium } from 'playwright';

const [base, workspace, scratch] = process.argv.slice(2);
if (!base || !workspace || !scratch) {
  console.error('usage: walk.mjs <base-url> <workspace-path> <scratch-dir>');
  process.exit(2);
}
// requestId is unknown until the "submit" step derives it from the URL
// the console's own router lands on after POST /requests succeeds (see
// that step, below) -- every later step's req()/waitForState reads this
// same top-level binding, which is why it is `let`, not `const`.
let requestId = '';
const HERE = path.dirname(fileURLToPath(import.meta.url));
const REQUEST_TEXT = fs.readFileSync(path.join(HERE, 'request.md'), 'utf8').trim();
// The same fixed verify command/preflight profile run.sh used to pass to
// `factoryd submit` directly -- now typed into the New request form's
// own Advanced section instead.
const VERIFY_COMMAND = "python3 -m unittest discover -s . -p 'test_*.py'";
const PREFLIGHT_PROFILE = 'brownfield';
const shots = path.join(scratch, 'shots');
fs.mkdirSync(shots, { recursive: true });
const results = [];
const MIN = 60_000;

const api = async (p) => {
  const r = await fetch(base + p, { headers: { Accept: 'application/json' } });
  if (!r.ok) throw new Error(`GET ${p}: HTTP ${r.status}`);
  return r.json();
};
const req = () => api(`/requests/${requestId}`);

const browser = await chromium.launch();
const ctx = await browser.newContext({ viewport: { width: 1400, height: 1000 } });
await ctx.addInitScript(() => {
  try { localStorage.setItem('factoryOperatorName', 'console-walk'); } catch {}
});
const page = await ctx.newPage();
const consoleErrors = [];
page.on('console', (m) => { if (m.type() === 'error') consoleErrors.push(m.text().slice(0, 300)); });

// Playwright locators wait for an element and scroll it into view; the walk
// only adds the "no reload" rule to the failure message.
const waitForText = async (re, timeout, what) => {
  try {
    await page.getByText(re).filter({ visible: true }).first().waitFor({ timeout });
  } catch {
    throw new Error(`timed out waiting for ${what ?? re} on screen (no reload allowed)`);
  }
};
const waitForState = async (states, timeout) => {
  const end = Date.now() + timeout;
  for (;;) {
    const r = await req();
    if (states.includes(r.state)) return r;
    if (Date.now() > end) throw new Error(`request stuck in ${r.state}, wanted ${states.join('|')}`);
    await page.waitForTimeout(3000);
  }
};
const main = () => page.getByRole('main');
const dialog = () => page.getByRole('dialog');
const button = (name, opts = {}) => main().getByRole('button', { name, exact: opts.exact ?? true }).first();
// Expands every oracle file tile the server lists, by name: approval stays
// disabled until each listed file has been shown.
const showOracleFiles = async (listingPath) => {
  const listing = await api(listingPath);
  const names = (listing.files ?? []).map((f) => f.name);
  for (const name of names) {
    const escaped = name.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
    await main().getByRole('button', { name: new RegExp(`^${escaped}`) }).click();
  }
  return { names, problems: listing.problems ?? [] };
};
const dialogButton = (name) => dialog().getByRole('button', { name, exact: true });

async function step(name, fn) {
  const started = Date.now();
  try {
    const note = await fn();
    results.push({ step: name, outcome: 'PASS', note: note ?? '', seconds: Math.round((Date.now() - started) / 1000) });
    console.log(`PASS ${name}${note ? ' -- ' + note : ''}`);
  } catch (e) {
    results.push({ step: name, outcome: 'FAIL', note: String(e.message || e), seconds: Math.round((Date.now() - started) / 1000) });
    console.log(`FAIL ${name} -- ${e.message || e}`);
    await page.screenshot({ path: path.join(shots, `${name}-FAIL.png`) }).catch(() => {});
    throw e;
  } finally {
    await page.screenshot({ path: path.join(shots, `${name}.png`) }).catch(() => {});
  }
}

const NOTE = 'Add an acceptance criterion: divide_numbers raises TypeError when either argument is not a number.';
const EDIT_LINE = 'Calling `subtract_numbers(a, a)` returns 0 for any number a.';

let exitCode = 0;
try {
  await step('submit', async () => {
    await page.goto(base + '/requests/new', { waitUntil: 'load' });
    await page.getByRole('heading', { name: 'New request' }).first().waitFor({ timeout: MIN });
    await page.getByLabel('Workspace path').fill(workspace);
    await page.getByLabel('Request', { exact: true }).fill(REQUEST_TEXT);
    await page.getByLabel('Draft oracles').check();
    await page.getByRole('button', { name: 'Advanced' }).click();
    await page.getByLabel(/Verify command/).fill(VERIFY_COMMAND);
    await page.getByLabel(/Preflight profile/).fill(PREFLIGHT_PROFILE);

    const before = page.url();
    await page.getByRole('button', { name: 'Submit request', exact: true }).click();
    const end = Date.now() + MIN;
    for (;;) {
      const url = page.url();
      if (url !== before && /\/requests\/[^/]+$/.test(new URL(url).pathname) && !url.endsWith('/requests/new')) {
        requestId = decodeURIComponent(new URL(url).pathname.split('/').pop());
        break;
      }
      if (Date.now() > end) throw new Error(`submit never landed on a request detail URL (still: ${url})`);
      await page.waitForTimeout(1000);
    }
    return `submitted ${requestId}`;
  });

  await step('board', async () => {
    await page.goto(base + '/', { waitUntil: 'load' });
    await waitForText(/Extend math_ops with two more operations/, MIN, 'the request row');
    if (await page.getByText(/worker is not running/).count()) throw new Error('board says the worker is not running while it is');
    // The full raw title is still in the row's tooltip, so assert the
    // visible short title instead: stripped and ellipsized.
    if (!(await page.getByText(/own unit tests \(use Python…/).count())) throw new Error('board row does not show the short title');
  });

  await step('spec-review', async () => {
    await page.getByRole('row', { name: /Extend math_ops with two more operations/ }).getByRole('link').first().click();
    await waitForState(['spec_review'], 10 * MIN);
    await waitForText(/Spec review/, 2 * MIN, 'spec review on the detail screen');
    await button('Approve').waitFor({ timeout: MIN });
    if (await button('Approve').isDisabled()) throw new Error('Approve disabled on loopback with no token');
  });

  await step('reject-note', async () => {
    const before = (await req()).history.length;
    await button('Request changes').click();
    await dialog().getByLabel('Reason').fill(NOTE);
    await dialogButton('Request changes').click();
    await dialog().waitFor({ state: 'detached' });
    const r = await waitForState(['spec_review'], 10 * MIN);
    if (r.history.length < before + 2) throw new Error('no redraft recorded');
    await waitForText(/Spec review/, 2 * MIN, 'the redraft back in spec review');
    const spec = r.spec ?? '';
    if (!/TypeError/.test(spec)) throw new Error('redraft ignored the operator note (no TypeError criterion)');
    return 'redraft contains TypeError';
  });

  await step('edit', async () => {
    const current = (await req()).spec;
    const lines = current.split('\n');
    const acc = lines.findIndex((l) => /^## Acceptance criteria/.test(l));
    if (acc < 0) throw new Error('spec has no Acceptance criteria section');
    let last = acc;
    for (let i = acc + 1; i < lines.length && !/^## /.test(lines[i]); i++) if (/^\d+\.\s/.test(lines[i])) last = i;
    const n = parseInt(lines[last], 10) + 1;
    lines.splice(last + 1, 0, `${n}. ${EDIT_LINE}`);
    const edited = lines.join('\n');

    await button('Edit').click();
    await main().getByRole('textbox', { name: 'Edit spec.md' }).fill(edited.replace(/\n+$/, ''));
    if (!(await button('Approve').isDisabled())) throw new Error('Approve still enabled with unsaved edits');
    await button('Save').click();
    await waitForText(new RegExp(`Acceptance criteria \\(${n}\\)`), MIN, 'the parsed criteria count');
    const after = (await req()).spec;
    if (!after.includes(EDIT_LINE)) throw new Error('saved spec does not contain the edited criterion');
    return `criterion ${n} saved`;
  });

  await step('approve-spec', async () => {
    await button('Approve').click();
    const sheet = await dialog().innerText();
    if (!/oracle_drafting|Drafting oracles/.test(sheet)) throw new Error('approve sheet does not name oracle drafting as the next state');
    await dialogButton('Approve').click();
    await waitForState(['oracle_drafting', 'oracle_review'], MIN);
  });

  await step('oracle', async () => {
    await waitForState(['oracle_review'], 15 * MIN);
    await waitForText(/Oracle review/, 2 * MIN, 'oracle review on screen');
    const r = await req();
    const approveAny = main().getByRole('button', { name: /^Approve( \(skip oracle\))?$/ }).first();
    await approveAny.waitFor({ timeout: MIN });
    if ((await approveAny.innerText()).includes('skip oracle')) {
      await approveAny.click();
      if (!/no request-level acceptance test/i.test(await dialog().innerText())) throw new Error('skip-oracle sheet does not state the consequence');
      await dialogButton('Approve').click();
      return `skipped (${r.oracle_draft?.status}; ${r.oracle_draft?.criteria?.length ?? 0} criterion verdicts)`;
    }
    const shown = await showOracleFiles(`/requests/${requestId}/oracle`);
    if (shown.problems.length) throw new Error(`oracle listing has problems that block approval: ${shown.problems.join('; ')}`);
    const approve = button('Approve');
    const end = Date.now() + MIN;
    while (await approve.isDisabled()) {
      if (Date.now() > end) throw new Error(`oracle Approve never enabled after showing ${shown.names.length} file(s)`);
      await page.waitForTimeout(1000);
    }
    await approve.click();
    await dialogButton('Approve').click();
    return `approved ${shown.names.length} oracle file(s): ${shown.names.join(', ')}`;
  });

  await step('approve-plan', async () => {
    await waitForState(['plan_review'], 15 * MIN);
    await waitForText(/Plan review/, 2 * MIN, 'plan review on screen');
    const r0 = await req();
    for (const t of r0.tickets ?? []) {
      await showOracleFiles(`/requests/${requestId}/tickets/${t.index}/oracle`).catch(() => {});
    }
    const approve = button('Approve');
    const end = Date.now() + MIN;
    while (await approve.isDisabled()) {
      if (Date.now() > end) throw new Error('plan Approve never enabled');
      await page.waitForTimeout(1000);
    }
    await approve.click();
    await dialogButton('Approve').click();
    await waitForState(['building', 'pr_review', 'halted', 'done'], MIN);
  });

  let firstRun = '';
  await step('build', async () => {
    const viewRun = main().getByRole('link', { name: 'View run', exact: true }).first();
    await viewRun.waitFor({ timeout: 5 * MIN });
    const r = await req();
    firstRun = r.tickets?.[0]?.run_id ?? '';
    await viewRun.click();
    await page.getByRole('link', { name: 'Open request' }).waitFor({ timeout: MIN });
    if (process.env.CONSOLE_WALK_TEMPORAL_UI) {
      await page.getByRole('link', { name: /Open in Temporal UI/ }).waitFor({ timeout: MIN })
        .catch(() => { throw new Error('no Open in Temporal UI link on a Temporal run'); });
    }
    const run = await api(`/runs/${firstRun}`);
    if (run.request_id !== requestId) throw new Error(`run.request_id ${run.request_id} != ${requestId}`);
    await waitForText(/Accepted|Quarantined/, 30 * MIN, 'the run to finish on screen');
    // Each Timeline stage is its own list item naming its status; the Build
    // row (it holds the agent notes) once vanished from it.
    await page.getByRole('list', { name: 'Timeline stages' }).getByRole('listitem')
      .filter({ hasText: /Build/ }).filter({ hasText: /Passed|Failed/ }).first().waitFor({ timeout: MIN })
      .catch(() => { throw new Error('timed out waiting for the Timeline Build row with its status'); });
    await page.getByRole('link', { name: 'Open request' }).click()
      .catch(() => { throw new Error('run page has no Open request link'); });
    await page.waitForURL(/\/requests\/[^/]+$/);
    return firstRun;
  });

  await step('end-state', async () => {
    const r = await waitForState(['halted', 'done', 'quarantined'], 5 * MIN);
    if (r.state === 'halted' && r.halt_kind === 'accepted_no_pr') {
      await waitForText(/Built and verified; no pull request was opened\./, 2 * MIN, 'the built-and-verified callout');
      const next = r.next_action ?? '';
      if (!/factoryd\//.test(next)) throw new Error(`Next does not name the branch: ${next}`);
      // After a release-policy denial, Next must lead with fixing the
      // policy -- never offer a bare retry the Detail says will be denied.
      if (/denied by release policy/.test(r.error ?? '') && !/^fix the release policy/.test(next)) {
        throw new Error(`Next offers a retry the denial says will fail: ${next}`);
      }
      // CONSOLE_WALK_EXPECT_RELEASE=allowed: the run's recorded release
      // decision must allow the PR.
      const decisions = [];
      const projects = path.join(scratch, 'data', 'projects');
      for (const p of fs.existsSync(projects) ? fs.readdirSync(projects) : []) {
        const dir = path.join(projects, p, 'release-decisions');
        for (const f of (fs.existsSync(dir) ? fs.readdirSync(dir) : []).filter((n) => n.endsWith('.json'))) {
          const full = path.join(dir, f);
          decisions.push({ mtime: fs.statSync(full).mtimeMs, body: JSON.parse(fs.readFileSync(full, 'utf8')) });
        }
      }
      decisions.sort((a, b) => b.mtime - a.mtime);
      const latest = decisions[0]?.body;
      if (process.env.CONSOLE_WALK_EXPECT_RELEASE === 'allowed' && latest?.allowed !== true) {
        throw new Error(`release decision not allowed: ${JSON.stringify(latest?.reasons ?? latest ?? 'none recorded')}`);
      }
      return `${next} [release allowed=${latest?.allowed}]`;
    }
    throw new Error(`unexpected end state ${r.state} (${r.error ?? ''})`);
  });

  await step('retry', async () => {
    // The run is accepted and only its pull request is missing, so Retry
    // re-attempts opening the PR against the same run: nothing is rebuilt.
    // The scratch clone has no remote, so the server refuses, and the
    // console must show the server's reason in the dialog and leave the
    // request as it was.
    await main().getByRole('button', { name: /^Retry request/ }).click();
    await dialog().getByLabel('Reason').fill('console-walk: exercise console retry');
    await dialog().getByRole('button', { name: /^Retry/ }).click();
    await dialog().getByRole('alert').getByText(/could not open a pull request/).waitFor({ timeout: 2 * MIN });
    const r = await req();
    if (r.state !== 'halted') throw new Error(`a refused retry moved the request to ${r.state}`);
    const run = r.tickets?.[0]?.run_id ?? '';
    if (run !== firstRun) throw new Error(`a PR-only retry started a new run: ${run}`);
    await dialog().getByRole('button', { name: 'Cancel', exact: true }).click();
    await main().getByText(/Built and verified; no pull request was opened\./).waitFor();
    return 'refused with the server\'s reason shown; same run, no rebuild';
  });
} catch {
  exitCode = 1;
} finally {
  if (consoleErrors.length) results.push({ step: 'browser-console-errors', outcome: 'INFO', note: consoleErrors.slice(0, 10).join(' | ') });
  fs.writeFileSync(path.join(scratch, 'results.json'), JSON.stringify({ requestId, results }, null, 2));
  await browser.close();
}
process.exit(exitCode);
