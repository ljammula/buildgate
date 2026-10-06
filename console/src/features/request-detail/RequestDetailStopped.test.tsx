// What the page says about a request that stopped, and about a ticket that is
// building: from the history, not from the state's name alone.
import { screen, within } from "@testing-library/react";

import { json } from "@/test/render";

import { openRequest, seedOperator, stubRadix } from "./testHarness";
import { historyWire, requestWire, runWire, ticketWire } from "./testRequests";

beforeAll(stubRadix);
beforeEach(seedOperator);

// A real request: spec drafting halted and was retried, then built, was
// quarantined, and the operator cancelled it.
const cancelledHistory = [
  historyWire({
    from: "submitted",
    to: "spec_drafting",
    at: "2026-10-06T03:55:07Z",
    by: "factory",
  }),
  historyWire({
    from: "spec_drafting",
    to: "halted",
    at: "2026-10-06T04:00:00Z",
    by: "factory",
    reason: "spec drafting failed",
  }),
  historyWire({
    from: "halted",
    to: "spec_drafting",
    at: "2026-10-06T04:03:35Z",
    by: "dogfood-operator",
    reason: "retried",
  }),
  historyWire({
    from: "spec_drafting",
    to: "spec_review",
    at: "2026-10-06T04:07:10Z",
    by: "factory",
    reason: "spec drafted",
  }),
  historyWire({
    from: "spec_review",
    to: "planning",
    at: "2026-10-06T04:07:21Z",
    by: "dogfood-operator",
    reason: "approved",
  }),
  historyWire({
    from: "planning",
    to: "plan_review",
    at: "2026-10-06T04:07:54Z",
    by: "factory",
    reason: "plan drafted",
  }),
  historyWire({
    from: "plan_review",
    to: "building",
    at: "2026-10-06T04:07:59Z",
    by: "dogfood-operator",
    reason: "approved",
  }),
  historyWire({
    from: "building",
    to: "quarantined",
    at: "2026-10-06T04:13:12Z",
    by: "factory",
    reason: "ticket 1/1 quarantined: required_files_changed",
  }),
  historyWire({
    from: "quarantined",
    to: "cancelled",
    at: "2026-10-06T04:13:53Z",
    by: "dogfood-operator",
    reason: "The modulo operation already exists; the ticket was redundant.",
  }),
];

test("a cancelled request shows its steps done up to where it stopped, says Cancelled, and prints why in the audit", async () => {
  openRequest(
    requestWire({ state: "cancelled", title: "Cancelled one", history: cancelledHistory }),
  );
  await screen.findByRole("heading", { level: 1, name: "Cancelled one" });

  const status = (step: string) =>
    screen.getByTestId(`pipeline-step-${step}`).getAttribute("data-status");
  expect(status("submitted")).toBe("done");
  expect(status("plan_review")).toBe("done");
  expect(status("building")).toBe("stopped");
  expect(status("pr_review")).toBe("pending");

  const outcome = screen.getByTestId("pipeline-outcome");
  expect(outcome).toHaveTextContent("Cancelled");
  expect(outcome).toHaveTextContent("dogfood-operator");
  expect(outcome).toHaveTextContent("The modulo operation already exists");

  const audit = screen.getByRole("region", { name: "Audit" });
  const lines = within(audit)
    .getAllByRole("listitem")
    .map((li) => li.textContent);
  expect(lines).toHaveLength(3);
  expect(lines[0]).toContain("Halted by factory at");
  expect(lines[1]).toContain("Quarantined by factory at");
  expect(lines[2]).toContain("Cancelled by dogfood-operator at");
  expect(lines[2]).toContain("the ticket was redundant.");
});

describe("a building ticket", () => {
  const building = (prUrl = "") =>
    requestWire({
      state: "building",
      title: "Building one",
      tickets: [ticketWire({ index: 1, runId: "run-1", prUrl })],
    });

  test("shows its run's current stage, round and last activity", async () => {
    openRequest(building(), {
      extra: [
        {
          on: "GET /runs/run-1",
          reply: json(
            runWire("run-running.json", {
              id: "run-1",
              current_stage: "verify",
              current_round: 2,
              max_rounds: 4,
              last_progress_at: "2026-09-10T09:46:00Z",
            }),
          ),
        },
      ],
    });
    const activity = await screen.findByTestId("ticket-activity");
    expect(activity).toHaveTextContent("Now: verify · round 2/4 · last activity");
  });

  test("links only a web pull request URL", async () => {
    openRequest(building("javascript:alert(1)"));
    await screen.findByTestId("ticket-card-1");
    expect(screen.getByText("javascript:alert(1)")).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "javascript:alert(1)" })).not.toBeInTheDocument();
  });
});
