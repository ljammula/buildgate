import { screen, within } from "@testing-library/react";

import { openRequest, seedOperator } from "./testHarness";
import { requestWire, runWire, ticketWire } from "./testRequests";

beforeEach(seedOperator);

const PR = "https://github.com/acme/app/pull/7";

function building(ticket: Record<string, unknown>) {
  return requestWire({
    state: "building",
    title: "Advised ticket",
    tickets: [
      { ...ticketWire({ index: 1, runId: "run-1", prUrl: PR, prState: "draft" }), ...ticket },
    ],
  });
}

const runRoute = {
  on: "GET /runs/run-1",
  reply: () => new Response(JSON.stringify(runWire("run-accepted.json", { id: "run-1" }))),
};

test("a ticket with no advice round shows no advice block", async () => {
  openRequest(building({}), { extra: [runRoute] });
  await screen.findByRole("heading", { level: 1, name: "Advised ticket" });

  expect(screen.queryByTestId("ticket-advice-rounds-1")).not.toBeInTheDocument();
});

test("an advice round under way and the ones that ended are on the ticket, apart from the review rounds", async () => {
  openRequest(
    building({
      advice: {
        of: "req-1-001-advice1",
        run_id: "req-1-001-advice2",
        since: "2026-10-27T10:00:00Z",
      },
      rounds: [
        {
          index: 1,
          kind: "advice",
          run_id: "req-1-001-advice1",
          outcome: "accepted",
          pushed: true,
          at: "2026-10-27T09:00:00Z",
        },
        {
          index: 2,
          kind: "advice",
          run_id: "req-1-001-advice0",
          outcome: "quarantined",
          pushed: false,
          error: "lint failed",
          at: "2026-10-27T09:30:00Z",
        },
      ],
    }),
    { extra: [runRoute] },
  );
  await screen.findByRole("heading", { level: 1, name: "Advised ticket" });
  const block = screen.getByTestId("ticket-advice-rounds-1");

  const items = within(block).getAllByRole("listitem");
  expect(items).toHaveLength(3);
  expect(items[0]).toHaveTextContent("checking the review's concerns about run req-1-001-advice1");
  expect(within(items[0]!).getByRole("link", { name: "Follow the run" })).toHaveAttribute(
    "href",
    "/runs/req-1-001-advice2",
  );
  expect(items[1]).toHaveTextContent("nothing pushed: the run it followed stands");
  expect(items[1]).toHaveTextContent("lint failed");
  expect(items[2]).toHaveTextContent("pushed: the pull request now has its commit");
  expect(within(items[2]!).getByRole("link", { name: "Advice round run" })).toHaveAttribute(
    "href",
    "/runs/req-1-001-advice1",
  );
  // Advice rounds are not review rounds.
  expect(screen.queryByTestId("ticket-rounds-1")).not.toBeInTheDocument();
});

test("a challenge round is listed apart from advice and review rounds, and the ledger names its kind", async () => {
  openRequest(
    building({
      rounds: [
        {
          index: 1,
          kind: "challenge",
          run_id: "req-1-001-challenge1",
          outcome: "accepted",
          pushed: false,
          at: "2026-10-28T09:00:00Z",
        },
      ],
      round_tags: [
        {
          n: 2,
          run_id: "req-1-001-challenge1",
          kind: "challenge",
          sha: "0123456789abcdef",
          ref: "refs/buildgate/x",
          outcome: "accepted",
          at: "2026-10-28T09:00:00Z",
        },
      ],
    }),
    { extra: [runRoute] },
  );
  await screen.findByRole("heading", { level: 1, name: "Advised ticket" });
  const block = screen.getByTestId("ticket-challenge-rounds-1");
  expect(within(block).getByRole("link", { name: "Challenge round run" })).toHaveAttribute(
    "href",
    "/runs/req-1-001-challenge1",
  );
  expect(screen.queryByTestId("ticket-advice-rounds-1")).not.toBeInTheDocument();
  expect(screen.queryByTestId("ticket-rounds-1")).not.toBeInTheDocument();
  expect(
    within(screen.getByTestId("round-ledger-1")).getByText("Challenge round"),
  ).toBeInTheDocument();
});
