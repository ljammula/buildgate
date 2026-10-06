import { screen, within } from "@testing-library/react";

import { openRequest, seedOperator } from "./testHarness";
import { requestWire, runWire, ticketWire } from "./testRequests";

beforeEach(seedOperator);

const PR = "https://github.com/acme/app/pull/6";

function prReview(ticket: Record<string, unknown>, nextAction = "") {
  return requestWire({
    state: "pr_review",
    title: "Liveness endpoint",
    next_action: nextAction,
    tickets: [
      { ...ticketWire({ index: 1, runId: "run-1", prUrl: PR, prState: "ready" }), ...ticket },
    ],
  });
}

const runRoute = {
  on: "GET /runs/run-1",
  reply: () => new Response(JSON.stringify(runWire("run-accepted.json", { id: "run-1" }))),
};

async function rounds() {
  await screen.findByRole("heading", { level: 1, name: "Liveness endpoint" });
  return screen.getByTestId("ticket-rounds-1");
}

test("a ticket with no rounds shows no rounds block", async () => {
  openRequest(prReview({}), { extra: [runRoute] });
  await screen.findByRole("heading", { level: 1, name: "Liveness endpoint" });

  expect(screen.queryByTestId("ticket-rounds-1")).not.toBeInTheDocument();
});

test("a round under way is on the ticket with a link to its run, and the Next line says to wait", async () => {
  openRequest(
    prReview(
      { active_round_run_id: "req-1-001-review1-x" },
      `review ${PR}: approve and merge it, or leave review comments`,
    ),
    { extra: [runRoute] },
  );
  const block = await rounds();

  const items = within(block).getAllByRole("listitem");
  expect(items).toHaveLength(1);
  expect(items[0]).toHaveTextContent("Round 1Buildingbuilding on the pull request's branch");
  expect(within(block).getByRole("link", { name: "Follow the run" })).toHaveAttribute(
    "href",
    "/runs/req-1-001-review1-x",
  );
  // The server's sentence is written without knowing a round started.
  expect(screen.getByTestId("next-action-banner")).toHaveTextContent(
    "A corrective round is building on ticket 1's pull request. Nothing to do until it ends; its run is on the ticket below.",
  );
});

test("ended rounds are listed newest first with outcome, effect, cause and run", async () => {
  const next = `corrective round 2 on ${PR} was quarantined and pushed nothing`;
  openRequest(
    prReview(
      {
        rounds: [
          { index: 1, run_id: "r1", outcome: "accepted", at: "2026-10-06T07:00:00Z", pushed: true },
          {
            index: 2,
            run_id: "r2",
            outcome: "quarantined",
            at: "2026-10-06T07:26:19Z",
            error: "policy gate did not pass: code_review <b>x</b>",
          },
        ],
      },
      next,
    ),
    { extra: [runRoute] },
  );
  const block = await rounds();

  const items = within(block).getAllByRole("listitem");
  expect(items).toHaveLength(2);
  expect(items[0]).toHaveTextContent("Round 2Quarantinednothing pushed");
  expect(items[0]).toHaveTextContent("policy gate did not pass: code_review <b>x</b>");
  expect(items[0]?.querySelector("b")).toBeNull();
  expect(items[1]).toHaveTextContent("Round 1Acceptedpushed to the pull request");
  expect(within(block).getByRole("link", { name: "Round 2 run" })).toHaveAttribute(
    "href",
    "/runs/r2",
  );
  // No round is under way, so the server's sentence stands.
  expect(screen.getByTestId("next-action-banner")).toHaveTextContent(next);
});

test("a round that needed a fix attempt links each earlier attempt's run", async () => {
  openRequest(
    prReview({
      rounds: [
        {
          index: 1,
          run_id: "r1-fix1",
          prior_run_ids: ["r1"],
          outcome: "accepted",
          at: "2026-10-06T07:00:00Z",
          pushed: true,
        },
      ],
    }),
    { extra: [runRoute] },
  );
  const block = await rounds();

  expect(within(block).getByRole("listitem")).toHaveTextContent(
    "After 1 attempt the review gate refused:",
  );
  expect(within(block).getByRole("link", { name: "Round 1 attempt 1 run" })).toHaveAttribute(
    "href",
    "/runs/r1",
  );
  expect(within(block).getByRole("link", { name: "Round 1 run" })).toHaveAttribute(
    "href",
    "/runs/r1-fix1",
  );
});

test("a round under way sits above the rounds that ended, numbered after them", async () => {
  openRequest(
    prReview({
      active_round_run_id: "req-1-001-review2-y",
      rounds: [{ index: 1, run_id: "r1", outcome: "quarantined", at: "2026-10-06T07:26:19Z" }],
    }),
    { extra: [runRoute] },
  );
  const block = await rounds();

  expect(
    within(block)
      .getAllByRole("listitem")
      .map((item) => item.textContent.slice(0, 7)),
  ).toEqual(["Round 2", "Round 1"]);
});
