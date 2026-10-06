// parity.mjs <flutter-base> <flutter-token> <react-base> <react-token> <parity-dir> [--only name...]
//
// Performs each operator scenario in the Flutter console and in the React
// console, records every write request (POST/PUT/DELETE/PATCH to the page's
// own origin) each page sends, and compares the two recordings and the
// server-side outcome. Scenarios are written in terms of the driver
// functions only (drivers/react.mjs, drivers/flutter.mjs).
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { isDeepStrictEqual } from "node:util";

import { chromium } from "playwright";

import * as flutterDriver from "./drivers/flutter.mjs";
import * as reactDriver from "./drivers/react.mjs";

const [flutterBase, flutterToken, reactBase, reactToken, parityDir, ...flags] =
  process.argv.slice(2);
if (!flutterBase || !flutterToken || !reactBase || !reactToken || !parityDir) {
  console.error(
    "usage: parity.mjs <flutter-base> <flutter-token> <react-base> <react-token> <parity-dir> [--only name...]",
  );
  process.exit(2);
}
// Names after --only, up to the next flag.
const only = [];
for (
  let i = flags.indexOf("--only") + 1;
  i > 0 && i < flags.length && !flags[i].startsWith("--");
  i++
)
  only.push(flags[i]);
const here = path.dirname(fileURLToPath(import.meta.url));
const shots = path.join(parityDir, "shots");
fs.mkdirSync(shots, { recursive: true });

const consoles = [
  { name: "flutter", base: flutterBase, token: flutterToken, driver: flutterDriver },
  { name: "react", base: reactBase, token: reactToken, driver: reactDriver },
];
for (const c of consoles) {
  c.workspace = path.join(parityDir, `workspace-${c.name}`);
  c.dataDir = path.join(parityDir, `data-${c.name}`);
}

// ------------------------------------------------------------ exceptions

function acceptedScenarios() {
  const file = path.join(here, "exceptions.md");
  if (!fs.existsSync(file)) return new Set();
  const names = new Set();
  for (const line of fs.readFileSync(file, "utf8").split("\n")) {
    if (!line.trim().startsWith("|")) continue;
    const first = line.split("|")[1]?.trim().replace(/^`|`$/g, "");
    if (!first || first === "Scenario" || /^-+$/.test(first)) continue;
    names.add(first);
  }
  return names;
}

// -------------------------------------------------------------- recording

/** Replace this console's own directories with placeholders, nothing else. */
function normalise(console_, text) {
  return text.split(console_.workspace).join("<WORKSPACE>").split(console_.dataDir).join("<DATA>");
}

function recorder(page, c) {
  const writes = [];
  const urlsWithToken = [];
  const origin = new URL(c.base).origin;
  page.on("request", (request) => {
    if (request.url().includes(c.token)) urlsWithToken.push(request.url());
    if (!["POST", "PUT", "DELETE", "PATCH"].includes(request.method())) return;
    const url = new URL(request.url());
    if (url.origin !== origin) return;
    const raw = request.postData();
    let body = null;
    if (raw !== null && raw !== undefined) {
      try {
        body = JSON.parse(normalise(c, raw));
      } catch {
        body = normalise(c, raw);
      }
    }
    const record = {
      method: request.method(),
      path: url.pathname + url.search,
      body,
      contentType: request.headers()["content-type"] ?? null,
      status: null,
    };
    record.answered = request
      .response()
      .then((response) => {
        record.status = response ? response.status() : null;
      })
      .catch(() => {});
    writes.push(record);
  });
  return {
    async finish() {
      await Promise.all(writes.map((w) => w.answered));
      return {
        writes: writes.map((write) => {
          const recorded = { ...write };
          delete recorded.answered;
          return recorded;
        }),
        urlsWithToken,
      };
    },
  };
}

async function api(c, pathname) {
  const response = await fetch(c.base + pathname);
  const text = await response.text();
  try {
    return { status: response.status, body: JSON.parse(text) };
  } catch {
    return { status: response.status, body: text };
  }
}

// -------------------------------------------------------------- comparing

const sortKeys = (value) =>
  Array.isArray(value)
    ? value.map(sortKeys)
    : value && typeof value === "object"
      ? Object.fromEntries(
          Object.keys(value)
            .sort()
            .map((k) => [k, sortKeys(value[k])]),
        )
      : value;

/** Readable differences between two JSON values, one line per differing leaf. */
function diffValues(a, b, at = "") {
  if (isDeepStrictEqual(sortKeys(a), sortKeys(b))) return [];
  const isObj = (v) => v && typeof v === "object";
  if (isObj(a) && isObj(b) && Array.isArray(a) === Array.isArray(b)) {
    const keys = [...new Set([...Object.keys(a), ...Object.keys(b)])].sort();
    return keys.flatMap((k) => {
      const here = Array.isArray(a) ? `${at}[${k}]` : at ? `${at}.${k}` : k;
      if (!(k in a)) return [`${here}: flutter <absent>, react ${JSON.stringify(b[k])}`];
      if (!(k in b)) return [`${here}: flutter ${JSON.stringify(a[k])}, react <absent>`];
      return diffValues(a[k], b[k], here);
    });
  }
  return [`${at || "(value)"}: flutter ${JSON.stringify(a)}, react ${JSON.stringify(b)}`];
}

function compareWrites(flutter, react) {
  const diffs = [];
  if (flutter.urlsWithToken.length)
    diffs.push(`flutter: start token in a URL: ${flutter.urlsWithToken[0]}`);
  if (react.urlsWithToken.length)
    diffs.push(`react: start token in a URL: ${react.urlsWithToken[0]}`);
  const label = (w) => `${w.method} ${w.path}`;
  if (flutter.writes.length !== react.writes.length) {
    diffs.push(
      `number of writes: flutter ${flutter.writes.length} [${flutter.writes.map(label).join(", ")}], ` +
        `react ${react.writes.length} [${react.writes.map(label).join(", ")}]`,
    );
  }
  const n = Math.min(flutter.writes.length, react.writes.length);
  for (let i = 0; i < n; i++) {
    const f = flutter.writes[i];
    const r = react.writes[i];
    const tag = `write ${i + 1}`;
    if (f.method !== r.method) diffs.push(`${tag} method: flutter ${f.method}, react ${r.method}`);
    if (f.path !== r.path) diffs.push(`${tag} path: flutter ${f.path}, react ${r.path}`);
    for (const line of diffValues(f.body, r.body)) diffs.push(`${tag} (${label(f)}) body.${line}`);
    if (f.contentType !== r.contentType) {
      diffs.push(`${tag} content-type: flutter ${f.contentType}, react ${r.contentType}`);
    }
    for (const [who, w] of [
      ["flutter", f],
      ["react", r],
    ]) {
      if (!/^application\/json\b/.test(w.contentType ?? "")) {
        diffs.push(`${tag} ${who} content-type is ${w.contentType}, not application/json`);
      }
    }
    if (f.status !== r.status)
      diffs.push(`${tag} response status: flutter ${f.status}, react ${r.status}`);
  }
  return diffs;
}

const outcomeOf = (res) =>
  res.status !== 200
    ? { httpStatus: res.status }
    : {
        state: res.body.state,
        approved_sha256: res.body.approved_sha256,
        last_rejection_reason: res.body.rejections?.at(-1)?.reason ?? null,
        last_rejection_by: res.body.rejections?.at(-1)?.by ?? null,
        spec: res.body.spec,
        ...(res.body.verify_command !== undefined
          ? { verify_command: res.body.verify_command, draft_oracles: res.body.draft_oracles }
          : {}),
      };

// -------------------------------------------------------------- scenarios

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

const oracleNames = async (c, listing) => (await api(c, listing)).body.files.map((f) => f.name);

/** A scenario: `affected` is the request whose server state is compared. */
const scenarios = [
  {
    name: "edit-spec",
    affected: "req-spec-review",
    run: async (d, page) => {
      await d.openRequest(page, "req-spec-review");
      await d.editSpec(page, validSpec);
    },
  },
  {
    name: "approve-spec",
    affected: "req-spec-review",
    run: async (d, page) => {
      await d.openRequest(page, "req-spec-review");
      await d.approve(page);
    },
  },
  {
    name: "request-changes",
    affected: "req-spec-review-b",
    run: async (d, page) => {
      await d.openRequest(page, "req-spec-review-b");
      await d.requestChanges(page, "Criterion 2 does not say which account.");
    },
  },
  {
    name: "oracle-approve",
    affected: "req-oracle-review",
    run: async (d, page, c) => {
      await d.openRequest(page, "req-oracle-review");
      await d.openOracleFiles(page, await oracleNames(c, "/requests/req-oracle-review/oracle"));
      await d.approveOracle(page);
    },
  },
  {
    name: "plan-approve",
    affected: "req-plan-review",
    run: async (d, page, c) => {
      await d.openRequest(page, "req-plan-review");
      await d.approvePlan(page, await oracleNames(c, "/requests/req-plan-review/tickets/1/oracle"));
    },
  },
  {
    name: "send-back",
    affected: "req-quarantined",
    run: async (d, page) => {
      await d.openRequest(page, "req-quarantined");
      await d.sendBack(page, "The plan misses the account scoping.");
    },
  },
  {
    name: "retry",
    affected: "req-halted",
    run: async (d, page) => {
      await d.openRequest(page, "req-halted");
      await d.retry(page, "gh is logged in again.");
    },
  },
  {
    name: "cancel",
    affected: "req-halted-b",
    run: async (d, page) => {
      await d.openRequest(page, "req-halted-b");
      await d.cancel(page, "No longer needed.");
    },
  },
  {
    name: "resume",
    affected: "req-every-field",
    run: async (d, page) => {
      await d.openRequest(page, "req-every-field");
      await d.resumeFromScratch(page);
    },
  },
  {
    name: "new-request",
    affected: null,
    run: async (d, page, c) => {
      await d.newRequest(page, {
        workspace: c.workspace,
        text: "Add a healthz endpoint\n\nIt returns 200 and the build SHA.",
        verifyCommand: "go test ./...",
        preflightProfile: "brownfield",
        draftOracles: true,
      });
    },
  },
];

// ----------------------------------------------------------------- runner

const channel = process.env.WALK_BROWSER_CHANNEL;
const browser = await chromium.launch({ ...(channel ? { channel } : {}), headless: true });
for (const c of consoles) {
  c.context = await browser.newContext({ viewport: { width: 1440, height: 900 } });
  await c.context.addInitScript(() => {
    try {
      window.localStorage.setItem("factoryOperatorName", "parity-operator");
    } catch {
      // Storage is unavailable: the consoles then prompt for the name.
    }
  });
  c.page = await c.context.newPage();
  c.page.setDefaultTimeout(15000);
  c.page.on("pageerror", (error) => (c.lastPageError = error.message));
  // The start-token link, once: the console stores the token and strips it.
  await c.page.goto(`${c.base}/#t=${c.token}`, { waitUntil: "load" });
  await c.page.waitForTimeout(c.name === "flutter" ? 4000 : 1000);
}

const accepted = acceptedScenarios();
let identical = 0;
let total = 0;
let failed = 0;

for (const scenario of scenarios) {
  if (only.length > 0 && !only.includes(scenario.name)) continue;
  total++;
  const recordings = {};
  const outcomes = {};
  let skipped = null;
  let error = null;
  for (const c of consoles) {
    const before = scenario.affected
      ? null
      : new Set((await api(c, "/requests")).body.map((r) => r.id));
    const record = recorder(c.page, c);
    try {
      await scenario.run(c.driver, c.page, c);
      await c.page.waitForTimeout(800);
    } catch (e) {
      if (/^not driven:/.test(e.message)) skipped = `${c.name}: ${e.message}`;
      else error = `${c.name}: ${e.message.split("\n")[0]}`;
    }
    recordings[c.name] = await record.finish();
    await c.page
      .screenshot({ path: path.join(shots, `${scenario.name}-${c.name}.png`), fullPage: true })
      .catch(() => undefined);
    let id = scenario.affected;
    if (!id) {
      const after = (await api(c, "/requests")).body.map((r) => r.id);
      id = after.find((x) => !before.has(x)) ?? null;
    }
    outcomes[c.name] = id ? outcomeOf(await api(c, `/requests/${id}`)) : null;
  }

  if (skipped) {
    console.log(`SKIP  ${scenario.name}\n      ${skipped}`);
    failed++;
    continue;
  }
  if (error) {
    console.log(`ERROR ${scenario.name}\n      ${error}`);
    failed++;
    continue;
  }
  const diffs = compareWrites(recordings.flutter, recordings.react);
  for (const line of diffValues(outcomes.flutter, outcomes.react)) diffs.push(`outcome.${line}`);
  if (diffs.length === 0) {
    identical++;
    console.log(`SAME  ${scenario.name}`);
  } else if (accepted.has(scenario.name)) {
    identical++;
    console.log(`DIFF (accepted)  ${scenario.name}\n      ${diffs.join("\n      ")}`);
  } else {
    failed++;
    console.log(`DIFF  ${scenario.name}\n      ${diffs.join("\n      ")}`);
  }
  fs.writeFileSync(
    path.join(parityDir, `recording-${scenario.name}.json`),
    JSON.stringify({ recordings, outcomes }, null, 2),
  );
}

await browser.close();
console.log(`\n${identical} of ${total} scenarios identical. Screenshots: ${shots}`);
process.exit(failed === 0 ? 0 : 1);
