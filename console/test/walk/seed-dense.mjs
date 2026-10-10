// seed-dense.mjs <walk-dir>
//
// Adds twenty requests to the seeded data directory for WALK_SEED=dense: a
// board with enough cards, long distinct titles and two projects to show how
// the layout holds up. Each is a copy of an already seeded request (so every
// state keeps the files it needs) with its own id, title and project.
import fs from "node:fs";
import path from "node:path";

const [walkDir] = process.argv.slice(2);
const requests = path.join(walkDir, "data", "requests");

const sources = [
  "req-spec-review",
  "req-oracle-review",
  "req-plan-review",
  "req-building",
  "req-quarantined",
  "req-halted",
  "req-done",
  "req-every-field",
];
const titles = [
  "Retry webhook delivery with exponential backoff and jitter",
  "Paginate the invoice export endpoint",
  "Reject expired coupons before the cart total is computed",
  "Move session storage from cookies to the signed token store",
  "Add structured audit logging to refund approvals",
  "Cache the currency conversion table for five minutes",
  "Fix off-by-one in the monthly usage rollup",
  "Validate shipping addresses against the postal service API",
  "Surface rate limit headers on every public endpoint",
  "Replace the polling worker with a queue consumer",
  "Redact card numbers from application error reports",
  "Make the search index rebuild resumable after a crash",
  "Send a receipt email when a subscription renews",
  "Throttle password reset requests per account",
  "Backfill missing tax codes on historical orders",
  "Return 409 instead of 500 on duplicate order submission",
  "Split the notification service into per-channel handlers",
  "Add a dry run flag to the data retention job",
  "Stop leaking stack traces in the health endpoint",
  "Show the next billing date on the account page",
];

for (const [i, title] of titles.entries()) {
  const source = sources[i % sources.length];
  const id = `req-dense-${String(i + 1).padStart(2, "0")}`;
  const dir = path.join(requests, id);
  fs.cpSync(path.join(requests, source), dir, { recursive: true });
  // A copied build would be a second live job on the same run.
  fs.rmSync(path.join(dir, "active_job.json"), { force: true });

  const file = path.join(dir, "request.json");
  const record = JSON.parse(
    fs
      .readFileSync(file, "utf8")
      .replaceAll(`requests/${source}/`, `requests/${id}/`)
      // Its own branch names; the run records are shared with the source on
      // purpose (this seed is for looking, not for a write step).
      .replaceAll(`factory/${source}-`, `factory/${id}-`),
  );
  record.id = id;
  record.project = i % 2 === 0 ? "app" : "billing-service";
  fs.writeFileSync(file, JSON.stringify(record, null, 2));

  const body = fs
    .readFileSync(path.join(dir, "request.md"), "utf8")
    .split("\n")
    .slice(1)
    .join("\n");
  fs.writeFileSync(path.join(dir, "request.md"), `${title}\n${body}`);
}
console.log(`walk: ${titles.length} dense requests in 2 projects`);
