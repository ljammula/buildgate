import { screen, within } from "@testing-library/react";

import { openRequest, seedOperator } from "./testHarness";
import { requestWire, runWire, ticketWire } from "./testRequests";

beforeEach(seedOperator);

const PR = "https://github.com/acme/app/pull/7";

function prReview(ticket: Record<string, unknown>) {
  return requestWire({
    state: "pr_review",
    title: "Stacked ticket",
    tickets: [
      { ...ticketWire({ index: 1, runId: "run-1", prUrl: PR, prState: "draft" }), ...ticket },
    ],
  });
}

const runRoute = {
  on: "GET /runs/run-1",
  reply: () => new Response(JSON.stringify(runWire("run-accepted.json", { id: "run-1" }))),
};

test("a ticket never restacked shows no restacks block", async () => {
  openRequest(prReview({}), { extra: [runRoute] });
  await screen.findByRole("heading", { level: 1, name: "Stacked ticket" });

  expect(screen.queryByTestId("ticket-restacks-1")).not.toBeInTheDocument();
});

test("a restack under way and one that ended are on the ticket, with their runs", async () => {
  openRequest(
    prReview({
      restack: {
        n: 2,
        branch: "buildgate/req-1/t1-restack2",
        onto_sha: "0123456789abcdef0123456789abcdef01234567",
        run_id: "req-1-001-restack2-x",
        since: "2026-10-25T10:00:00Z",
      },
      rounds: [
        {
          index: 1,
          kind: "restack",
          run_id: "req-1-001-restack1-x",
          outcome: "accepted",
          pushed: true,
          at: "2026-10-25T09:00:00Z",
        },
        {
          index: 2,
          kind: "",
          run_id: "req-1-001-review2-x",
          outcome: "accepted",
          pushed: true,
          at: "2026-10-25T09:30:00Z",
        },
      ],
    }),
    { extra: [runRoute] },
  );
  await screen.findByRole("heading", { level: 1, name: "Stacked ticket" });
  const block = screen.getByTestId("ticket-restacks-1");

  const items = within(block).getAllByRole("listitem");
  expect(items).toHaveLength(2);
  expect(items[0]).toHaveTextContent("Restack 2");
  expect(items[0]).toHaveTextContent("replaying its commits onto 0123456789ab");
  expect(within(items[0]!).getByRole("link", { name: "Follow the run" })).toHaveAttribute(
    "href",
    "/runs/req-1-001-restack2-x",
  );
  expect(items[1]).toHaveTextContent("the pull request now has the replayed commits");
  expect(within(items[1]!).getByRole("link", { name: "Restack run" })).toHaveAttribute(
    "href",
    "/runs/req-1-001-restack1-x",
  );
  // The review round stays among the review rounds, not the restacks.
  expect(within(screen.getByTestId("ticket-rounds-1")).getAllByRole("listitem")).toHaveLength(1);
});
