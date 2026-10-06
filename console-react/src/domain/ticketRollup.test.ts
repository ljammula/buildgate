import type { JsonObject } from "@/domain/decode";
import {
  type RequestSummary,
  type RequestTicket,
  decodeRequestSummary,
  decodeRequestTicket,
} from "@/domain/request";
import { computeTicketRollup, ticketStatus } from "@/domain/ticketRollup";

function ticketOf(args: { index: number; runId?: string; prState?: string }): RequestTicket {
  const prState = args.prState ?? "";
  const body: JsonObject = {
    index: args.index,
    run_id: args.runId ?? "",
    pr_state: prState,
    pr_url: prState === "" ? "" : `https://github.com/acme/app/pull/${args.index}`,
  };
  return decodeRequestTicket(body, "test");
}

function requestOf(
  args: { state?: string; ticketIndex?: number; haltKind?: string } = {},
): RequestSummary {
  return decodeRequestSummary(
    {
      id: "req-1",
      workspace: "ws",
      project: "proj",
      state: args.state ?? "building",
      submitted_at: "",
      updated_at: "",
      ticket_index: args.ticketIndex ?? 1,
      halt_kind: args.haltKind ?? "",
    },
    "test",
  );
}

const labelled = (entries: { count: number; label: string }[]) =>
  entries.map((e) => `${e.count} ${e.label}`);

describe("ticketStatus", () => {
  test("a ticket with a prState uses the shared status vocabulary", () => {
    expect(ticketStatus(ticketOf({ index: 1, prState: "changes_requested" }), requestOf())).toBe(
      "needsHuman",
    );
    expect(ticketStatus(ticketOf({ index: 1, prState: "merged" }), requestOf())).toBe("done");
  });

  test("a ticket with a runId but no prState counts as building (Status.working) -- an acknowledged approximation, see ticketStatus doc comment", () => {
    expect(
      ticketStatus(ticketOf({ index: 1, runId: "run-1" }), requestOf({ ticketIndex: 1 })),
    ).toBe("working");
  });

  test('a ticket with neither a runId nor a prState is unknown ("queued")', () => {
    expect(ticketStatus(ticketOf({ index: 1 }), requestOf({ ticketIndex: 1 }))).toBe("unknown");
  });

  // C7 (operator demo, 2026-09-26): a ticket the request has already
  // moved past still had a runId and no prState (no PR was ever opened
  // for it), so it read as "1 building" on an otherwise-accepted
  // request. Its index below the request's current ticketIndex is
  // itself the "this one is done" signal, independent of runId/prState.
  test("a ticket below the request current ticket index is done", () => {
    expect(
      ticketStatus(ticketOf({ index: 1, runId: "run-1" }), requestOf({ ticketIndex: 2 })),
    ).toBe("done");
  });

  test("the current ticket is done once the request reaches pr_review", () => {
    expect(
      ticketStatus(
        ticketOf({ index: 2, runId: "run-2" }),
        requestOf({ state: "pr_review", ticketIndex: 2 }),
      ),
    ).toBe("done");
  });

  test("the current ticket is done once the request is done", () => {
    expect(
      ticketStatus(
        ticketOf({ index: 2, runId: "run-2" }),
        requestOf({ state: "done", ticketIndex: 2 }),
      ),
    ).toBe("done");
  });

  test("the current ticket is done when the request is accepted awaiting its PR", () => {
    expect(
      ticketStatus(
        ticketOf({ index: 2, runId: "run-2" }),
        requestOf({ state: "halted", haltKind: "accepted_no_pr", ticketIndex: 2 }),
      ),
    ).toBe("done");
  });

  test("the current ticket still building stays Status.working", () => {
    expect(
      ticketStatus(
        ticketOf({ index: 2, runId: "run-2" }),
        requestOf({ state: "building", ticketIndex: 2 }),
      ),
    ).toBe("working");
  });
});

describe("computeTicketRollup", () => {
  test("an empty ticket list produces no entries", () => {
    expect(computeTicketRollup([], requestOf())).toEqual([]);
  });

  test("groups tickets by status and counts them, most-actionable first", () => {
    const tickets = [
      ticketOf({ index: 1, prState: "merged" }),
      ticketOf({ index: 2, prState: "merged" }),
      ticketOf({ index: 3, prState: "changes_requested" }),
      ticketOf({ index: 4, runId: "run-4" }),
      ticketOf({ index: 5, runId: "run-5" }),
    ];

    // ticketIndex 4 (not past ticket 4 or 5 yet, and still building) --
    // keeps both runId-only tickets counted as "building", matching this
    // test's pre-existing expectation.
    const entries = computeTicketRollup(tickets, requestOf({ ticketIndex: 4 }));

    expect(entries.map((e) => e.label)).toEqual(["changes requested", "building", "done"]);
    expect(entries.map((e) => e.count)).toEqual([1, 2, 2]);
  });
});

test("a PR state with no PR URL (an unopened PR) is not a PR status", () => {
  const ticket = decodeRequestTicket({ index: 1, run_id: "run-1", pr_state: "draft" }, "test");
  expect(ticketStatus(ticket, requestOf({ ticketIndex: 1 }))).toBe("working");
});

test("the current ticket of a stopped request counts as stopped, not building", () => {
  for (const state of ["quarantined", "cancelled", "halted"]) {
    expect(
      ticketStatus(ticketOf({ index: 1, runId: "run-1" }), requestOf({ state, ticketIndex: 1 })),
      state,
    ).toBe("failed");
  }
});

test("tickets whose PR is ready roll up as awaiting review (needs you), not building", () => {
  const entries = computeTicketRollup(
    [
      ticketOf({ index: 1, runId: "run-1", prState: "ready" }),
      ticketOf({ index: 2, runId: "run-2", prState: "ready" }),
    ],
    requestOf({ state: "pr_review", ticketIndex: 2 }),
  );
  expect(labelled(entries)).toEqual(["2 awaiting review"]);
});

test("a stacked PR rolls up as waiting on its base PR", () => {
  const entries = computeTicketRollup(
    [
      ticketOf({ index: 1, runId: "run-1", prState: "ready" }),
      ticketOf({ index: 2, runId: "run-2", prState: "stacked" }),
    ],
    requestOf({ state: "pr_review", ticketIndex: 2 }),
  );
  expect(new Set(labelled(entries))).toEqual(
    new Set(["1 awaiting review", "1 waiting on base PR"]),
  );
});

test("an open (not yet ready) PR still rolls up as in review", () => {
  const entries = computeTicketRollup(
    [ticketOf({ index: 1, runId: "run-1", prState: "open" })],
    requestOf({ state: "pr_review", ticketIndex: 1 }),
  );
  expect(labelled(entries)).toEqual(["1 in review"]);
});

test("a building ticket beside a ready PR shows both buckets", () => {
  const entries = computeTicketRollup(
    [
      ticketOf({ index: 1, runId: "run-1", prState: "ready" }),
      ticketOf({ index: 2, runId: "run-2" }),
    ],
    requestOf({ ticketIndex: 2 }),
  );
  expect(new Set(labelled(entries))).toEqual(new Set(["1 awaiting review", "1 building"]));
});
