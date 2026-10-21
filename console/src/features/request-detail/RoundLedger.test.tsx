import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { json } from "@/test/render";

import { openRequest, seedOperator } from "./testHarness";
import { requestWire, runWire, ticketWire } from "./testRequests";

beforeEach(seedOperator);

const SHA = "a".repeat(40);

function tag(n: number, over: Record<string, unknown> = {}) {
  return {
    n,
    run_id: `run-${n}`,
    kind: n === 1 ? "build" : "conformity",
    sha: SHA,
    ref: `refs/buildgate/rounds/req-1/t1/r${n}`,
    outcome: "quarantined",
    failed_checks: ["spec_conformity"],
    at: "2026-10-20T10:00:00Z",
    ...over,
  };
}

function quarantined(ticket: Record<string, unknown> = {}, state = "quarantined") {
  return requestWire({
    state,
    title: "Ledger",
    ticket_index: 1,
    tickets: [
      {
        ...ticketWire({ index: 1, runId: "run-2" }),
        round_tags: [tag(1), tag(2)],
        ...ticket,
      },
    ],
  });
}

const runRoute = {
  on: "GET /runs/run-2",
  reply: () => new Response(JSON.stringify(runWire("run-accepted.json", { id: "run-2" }))),
};

const heading = () => screen.findByRole("heading", { level: 1, name: "Ledger" });

test("each round of the ledger is listed with its commit, failed checks and ref", async () => {
  openRequest(quarantined(), { extra: [runRoute] });
  await heading();

  const ledger = screen.getByTestId("round-ledger-1");
  const items = within(ledger).getAllByRole("listitem");
  expect(items).toHaveLength(2);
  expect(items[0]).toHaveTextContent("Round 1build");
  expect(items[0]).toHaveTextContent("aaaaaaaaaaaa");
  expect(items[0]).toHaveTextContent("failed: spec_conformity");
  expect(items[0]).toHaveTextContent("refs/buildgate/rounds/req-1/t1/r1");
  expect(items[1]).toHaveTextContent("Round 2conformity");
});

test("a ticket with no ledger shows no rounds block", async () => {
  openRequest(quarantined({ round_tags: [] }), { extra: [runRoute] });
  await heading();
  expect(screen.queryByTestId("round-ledger-1")).not.toBeInTheDocument();
});

test("Retry from this round asks for a reason and sends the round number", async () => {
  const { server } = openRequest(quarantined(), {
    extra: [
      runRoute,
      {
        on: "POST /requests/req-1/retry",
        reply: json(requestWire({ state: "building", title: "Ledger" })),
      },
    ],
  });
  await heading();

  const buttons = within(screen.getByTestId("round-ledger-1")).getAllByRole("button", {
    name: "Retry from this round",
  });
  expect(buttons[0]).toBeEnabled();
  await userEvent.click(buttons[0]!);
  const dialog = await screen.findByRole("dialog", { name: "Retry from round 1" });
  await userEvent.type(within(dialog).getByLabelText("Reason"), "round 1 was the better one");
  await userEvent.click(within(dialog).getByRole("button", { name: "Retry" }));

  await waitFor(() => {
    expect(server.sent("POST /requests/req-1/retry")).toHaveLength(1);
  });
  expect(server.sent("POST /requests/req-1/retry")[0]?.body).toEqual({
    reason: "round 1 was the better one",
    by: "operator",
    from: "1",
  });
});

test("the button is off while the request builds, and once the ticket's branch was pushed", async () => {
  openRequest(quarantined({}, "building"), { extra: [runRoute] });
  await heading();
  for (const button of within(screen.getByTestId("round-ledger-1")).getAllByRole("button", {
    name: "Retry from this round",
  })) {
    expect(button).toBeDisabled();
  }
});

test("a pushed ticket cannot be retried from a round", async () => {
  openRequest(quarantined({ pr_url: "https://github.com/acme/app/pull/6", pr_state: "open" }), {
    extra: [runRoute],
  });
  await heading();
  const [first] = within(screen.getByTestId("round-ledger-1")).getAllByRole("button", {
    name: "Retry from this round",
  });
  expect(first).toBeDisabled();
  expect(first).toHaveAttribute("title", expect.stringContaining("close its pull request"));
});
