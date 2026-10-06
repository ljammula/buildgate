import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { type FakeRoute, apiErrorResponse, json, renderApp, sseResponse } from "@/test/render";

import { BoardScreen } from "./BoardScreen";
import { requestJson, ticketJson } from "@/test/requestFixtures";

type RequestOptions = Parameters<typeof requestJson>[0];

function board(requests: RequestOptions[], extra: FakeRoute[] = []): FakeRoute[] {
  return [
    { on: "GET /requests", reply: () => json(requests.map(requestJson)) },
    { on: "GET /requests/events", reply: sseResponse("state") },
    {
      on: "GET /queue-run",
      reply: () => json({ state: "alive", last_heartbeat: "2026-09-24T09:00:00Z" }),
    },
    ...extra,
  ];
}

const costSummary = (extra: Record<string, unknown>) => ({
  spec: 0,
  plan: 0,
  runs: 0,
  total: 0,
  currency: "usd",
  complete: true,
  tokens_complete: true,
  ...extra,
});

test("request list renders each stage group and the waiting badge", async () => {
  const waitingSince = new Date(Date.now() - 45 * 60_000).toISOString();
  renderApp(<BoardScreen />, {
    server: board([
      { id: "req-review", state: "spec_review", title: "Add idempotency keys", waitingSince },
      {
        id: "req-working",
        state: "building",
        title: "Retry logic",
        ticketIndex: 1,
        ticketCount: 2,
      },
      { id: "req-done", state: "done", title: "Done request" },
    ]),
  });
  expect(await screen.findByText("Add idempotency keys")).toBeInTheDocument();
  expect(screen.getByText("Retry logic")).toBeInTheDocument();
  expect(screen.getByText("Done request")).toBeInTheDocument();
  expect(screen.getAllByText(/Waiting on you/)).toHaveLength(1);
  expect(screen.getByText("Ticket 1 / 2")).toBeInTheDocument();

  // Review sorts before working sorts before done.
  const links = screen
    .getAllByRole("link")
    .map((link) => link.textContent)
    .filter((text) => !text.startsWith("req-"));
  expect(links).toEqual(["Add idempotency keys", "Retry logic", "Done request"]);
});

test("a row links to its request", async () => {
  renderApp(<BoardScreen />, {
    server: board([{ id: "req-a", state: "done", title: "Linked" }]),
  });
  expect(await screen.findByRole("link", { name: "Linked" })).toHaveAttribute(
    "href",
    "/requests/req-a",
  );
});

test("a refresh failure surfaces a stale-data warning, not silence", async () => {
  const server = renderApp(<BoardScreen />, {
    server: board([{ id: "req-1", state: "spec_review", title: "Add idempotency keys" }]),
  }).server;
  expect(await screen.findByText("Add idempotency keys")).toBeInTheDocument();
  expect(screen.queryByTestId("request-list-stale-banner")).not.toBeInTheDocument();

  server.set("GET /requests", () => apiErrorResponse(500, "factory is unreachable"));
  await userEvent.click(screen.getByRole("button", { name: "Refresh" }));

  expect(await screen.findByTestId("request-list-stale-banner")).toBeInTheDocument();
  expect(screen.getByText("Add idempotency keys")).toBeInTheDocument();
  expect(server.sent("GET /requests")).toHaveLength(2);
});

test("a first load that fails shows the error, with no board", async () => {
  renderApp(<BoardScreen />, {
    server: [{ on: "GET /requests", reply: () => apiErrorResponse(500, "boom") }],
  });
  expect(await screen.findByRole("alert")).toHaveTextContent("Request failed (500)");
});

test("section headers group requests into Needs you / Working / Finished with counts", async () => {
  renderApp(<BoardScreen />, {
    server: board([
      { id: "req-review", state: "spec_review", title: "Needs review" },
      { id: "req-working", state: "building", title: "In progress" },
      { id: "req-done", state: "done", title: "Wrapped up" },
      { id: "req-failed", state: "quarantined", title: "Blocked" },
    ]),
  });
  expect(await screen.findByText("Needs you (1)")).toBeInTheDocument();
  expect(screen.getByText("Working (1)")).toBeInTheDocument();
  // done + quarantined both collapse into "Finished", not a fifth bucket.
  expect(screen.getByText("Finished (2)")).toBeInTheDocument();
});

test("a halted request lands in Needs you, not Finished, and counts toward the tab-title badge", async () => {
  renderApp(<BoardScreen />, {
    server: board([
      { id: "req-halted", state: "halted", title: "Needs a retry" },
      { id: "req-done", state: "done", title: "Wrapped up" },
    ]),
  });
  expect(await screen.findByText("Needs you (1)")).toBeInTheDocument();
  expect(screen.getByText("Finished (1)")).toBeInTheDocument();
  expect(screen.getByText("Needs a retry")).toBeInTheDocument();
  await waitFor(() => {
    expect(document.title).toBe("(1) Factory Console");
  });
});

test("the tab title shows (?) once a poll fails", async () => {
  const { server } = renderApp(<BoardScreen />, {
    server: board([{ id: "req-a", state: "done", title: "A" }]),
  });
  await screen.findByText("A");
  server.set("GET /requests", () => apiErrorResponse(500, "down"));
  await userEvent.click(screen.getByRole("button", { name: "Refresh" }));
  await waitFor(() => {
    expect(document.title).toBe("(?) Factory Console");
  });
});

test("a project filter chip hides requests from other projects", async () => {
  const { location } = renderApp(<BoardScreen />, {
    server: board([
      { id: "req-a", state: "done", project: "checkouts", title: "Checkout fix" },
      { id: "req-b", state: "done", project: "billing", title: "Billing fix" },
    ]),
  });
  expect(await screen.findByText("Checkout fix")).toBeInTheDocument();
  expect(screen.getByText("Billing fix")).toBeInTheDocument();

  await userEvent.click(screen.getByRole("button", { name: "checkouts" }));

  expect(screen.getByText("Checkout fix")).toBeInTheDocument();
  expect(screen.queryByText("Billing fix")).not.toBeInTheDocument();
  expect(screen.getByRole("button", { name: "checkouts" })).toHaveAttribute("aria-pressed", "true");
  expect(location()).toBe("/?project=checkouts");
});

test("free-text search filters the board by id/title/workspace", async () => {
  const { location } = renderApp(<BoardScreen />, {
    server: board([
      { id: "req-a", state: "done", title: "Add idempotency keys" },
      { id: "req-b", state: "done", title: "Retry logic" },
    ]),
  });
  await screen.findByText("Retry logic");
  await userEvent.type(screen.getByRole("searchbox"), "idempotency");

  expect(screen.getByText("Add idempotency keys")).toBeInTheDocument();
  expect(screen.queryByText("Retry logic")).not.toBeInTheDocument();
  expect(location()).toBe("/?q=idempotency");
});

test("the filters are read from the URL", async () => {
  renderApp(<BoardScreen />, {
    path: "/?group=working&project=app&q=ret",
    server: board([
      { id: "req-a", state: "building", project: "app", title: "Retry logic" },
      { id: "req-b", state: "building", project: "app", title: "Other work" },
      { id: "req-c", state: "done", project: "app", title: "Retry done" },
    ]),
  });
  expect(await screen.findByText("Retry logic")).toBeInTheDocument();
  expect(screen.queryByText("Other work")).not.toBeInTheDocument();
  expect(screen.queryByText("Retry done")).not.toBeInTheDocument();
  expect(screen.getByRole("searchbox")).toHaveValue("ret");
  expect(screen.getByRole("button", { name: "Working" })).toHaveAttribute("aria-pressed", "true");
});

test("a filter that matches nothing says so", async () => {
  renderApp(<BoardScreen />, {
    path: "/?q=zzz",
    server: board([{ id: "req-a", state: "done", title: "A" }]),
  });
  expect(await screen.findByText("No requests found.")).toBeInTheDocument();
});

test("the needs-you banner selects the needs-you filter", async () => {
  const { location } = renderApp(<BoardScreen />, {
    server: board([
      { id: "req-a", state: "spec_review", title: "Review me" },
      { id: "req-b", state: "done", title: "Old" },
    ]),
  });
  const banner = await screen.findByTestId("needs-you-banner");
  expect(banner).toHaveTextContent("1 request(s) waiting for your review");
  await userEvent.click(within(banner).getByRole("button", { name: "View" }));
  expect(location()).toBe("/?group=needs-you");
  expect(screen.queryByText("Old")).not.toBeInTheDocument();
  // Already looking at it: the banner goes.
  expect(screen.queryByTestId("needs-you-banner")).not.toBeInTheDocument();
});

test("a building request with tickets shows the fan-out roll-up strip", async () => {
  renderApp(<BoardScreen />, {
    server: board([
      {
        id: "req-a",
        state: "building",
        title: "Multi-ticket request",
        tickets: [
          ticketJson({ index: 1, prState: "merged" }),
          ticketJson({ index: 2, prState: "changes_requested" }),
          ticketJson({ index: 3, runId: "run-3" }),
        ],
      },
    ]),
  });
  expect(await screen.findByText("1 done")).toBeInTheDocument();
  expect(screen.getByText("1 changes requested")).toBeInTheDocument();
  expect(screen.getByText("1 building")).toBeInTheDocument();
  expect(screen.getByText("PR merged")).toBeInTheDocument();
});

test("the inline token figure renders — for missing evidence, never 0", async () => {
  renderApp(<BoardScreen />, {
    server: board([{ id: "req-a", state: "spec_drafting", title: "No evidence yet" }]),
  });
  expect(await screen.findByTestId("request-token-total")).toHaveTextContent(/^—$/);
});

test("the board row shows the server cost_summary usage figure when present", async () => {
  renderApp(<BoardScreen />, {
    server: board([
      {
        id: "req-a",
        state: "building",
        title: "Has a cost rollup",
        costSummary: costSummary({
          total: 4.5,
          tokens: 478300,
          by_model: [{ model: "gpt-5.6-luna", tokens: 478300 }],
        }),
      },
    ]),
  });
  expect(await screen.findByTestId("request-cost-total")).toHaveTextContent(
    "gpt-5.6-luna · 478.3k tokens",
  );
  expect(screen.queryByTestId("request-token-total")).not.toBeInTheDocument();
});

test('an incomplete cost_summary renders "≥ " tokens, not an exact figure', async () => {
  renderApp(<BoardScreen />, {
    server: board([
      {
        id: "req-a",
        state: "building",
        title: "Incomplete rollup",
        costSummary: costSummary({
          total: 2,
          complete: false,
          tokens: 500,
          tokens_complete: false,
        }),
      },
    ]),
  });
  expect(await screen.findByTestId("request-cost-total")).toHaveTextContent(/^≥ 500 tokens$/);
});

test("a request rejected at least once shows a ↻N marker on the board row", async () => {
  const rejection = (at: string, reason: string) => ({
    by: "jane",
    at,
    reason,
    from_state: "spec_review",
  });
  renderApp(<BoardScreen />, {
    server: board([
      {
        id: "req-a",
        state: "spec_review",
        title: "Rejected twice",
        rejections: [
          rejection("2026-09-10T09:00:00Z", "too broad"),
          rejection("2026-09-11T09:00:00Z", "still too broad"),
        ],
      },
    ]),
  });
  expect(await screen.findByTestId("rejection-marker")).toHaveTextContent("↻2");
});

test("a request never rejected shows no ↻ marker", async () => {
  renderApp(<BoardScreen />, {
    server: board([{ id: "req-a", state: "spec_review", title: "Never rejected" }]),
  });
  await screen.findByText("Never rejected");
  expect(screen.queryByTestId("rejection-marker")).not.toBeInTheDocument();
});

test("the board row title is the derived short title, markdown/backticks stripped, with the full raw title in a tooltip", async () => {
  renderApp(<BoardScreen />, {
    server: board([{ id: "req-a", state: "spec_review", title: "`Add sub.py` and **div.py**" }]),
  });
  // The stripped, plain-text form is what's shown...
  expect(await screen.findByText("Add sub.py and div.py")).toBeInTheDocument();
  // ...and it is still searchable by the raw, unstripped text.
  await userEvent.type(screen.getByRole("searchbox"), "`Add sub.py`");
  const link = screen.getByRole("link", { name: "Add sub.py and div.py" });
  // The full raw title is one hover away.
  expect(link).toHaveAttribute("title", "`Add sub.py` and **div.py**");
});

describe("release policy warning banner", () => {
  test("shows when the server reports a deny-all release policy", async () => {
    renderApp(<BoardScreen />, {
      server: board([]),
      config: {
        releasePolicyWarning:
          "release policy denies every PR unconditionally (release_max_files_changed=0) -- add release_max_files_changed, release_max_insertions, and release_rollback_plan to your session config",
      },
    });
    expect(await screen.findByTestId("release-policy-warning-banner")).toHaveTextContent(
      "Release policy denies every PR",
    );
  });

  test("is hidden when the server reports a usable release policy", async () => {
    renderApp(<BoardScreen />, { server: board([]) });
    await screen.findByText("No requests found.");
    expect(screen.queryByTestId("release-policy-warning-banner")).not.toBeInTheDocument();
  });
});

describe("worker heartbeat banner", () => {
  function withQueueRun(reply: FakeRoute["reply"], requests: RequestOptions[] = []) {
    return board(requests, [{ on: "GET /queue-run", reply }]);
  }

  test("shows when worker is stale", async () => {
    renderApp(<BoardScreen />, {
      server: withQueueRun(() => json({ state: "stale", last_heartbeat: "2026-09-24T09:00:00Z" })),
    });
    const banner = await screen.findByTestId("worker-down-banner");
    expect(banner).toHaveTextContent("worker is not running");
    expect(banner).toHaveTextContent("start `factoryd worker`");
  });

  test("shows when worker is absent and a request is in a worker-dependent working state", async () => {
    renderApp(<BoardScreen />, {
      server: withQueueRun(() => json({ state: "absent" }), [{ id: "req-a", state: "building" }]),
    });
    const banner = await screen.findByTestId("worker-down-banner");
    expect(banner).toHaveTextContent("no worker has run");
    expect(banner).toHaveTextContent("start `factoryd worker`");
  });

  test("is hidden when worker is absent but no request is working", async () => {
    renderApp(<BoardScreen />, {
      server: withQueueRun(() => json({ state: "absent" }), [{ id: "req-a", state: "done" }]),
    });
    await screen.findByText("Finished (1)");
    expect(screen.queryByTestId("worker-down-banner")).not.toBeInTheDocument();
  });

  test("is hidden when worker is alive", async () => {
    renderApp(<BoardScreen />, {
      server: withQueueRun(() => json({ state: "alive", last_heartbeat: "2026-09-24T09:00:00Z" })),
    });
    await screen.findByText("No requests found.");
    expect(screen.queryByTestId("worker-down-banner")).not.toBeInTheDocument();
  });

  test("is hidden on a 404 (an older server predating GET /queue-run) rather than false-alarming", async () => {
    renderApp(<BoardScreen />, {
      server: withQueueRun(() => apiErrorResponse(404, "not found")),
    });
    await screen.findByText("No requests found.");
    expect(screen.queryByTestId("worker-down-banner")).not.toBeInTheDocument();
  });

  test("Retry asks the server again", async () => {
    const { server } = renderApp(<BoardScreen />, {
      server: withQueueRun(() => json({ state: "stale", last_heartbeat: "" })),
    });
    const banner = await screen.findByTestId("worker-down-banner");
    await userEvent.click(within(banner).getByRole("button", { name: "Retry" }));
    await waitFor(() => {
      expect(server.sent("GET /queue-run")).toHaveLength(2);
    });
  });
});

describe("freshness indicator", () => {
  test('shows "Live" once the SSE connection is open', async () => {
    renderApp(<BoardScreen />, { server: board([]) });
    await screen.findByText("No requests found.");
    await waitFor(() => {
      expect(screen.getByTestId("board-freshness")).toHaveTextContent("Live");
    });
  });

  test('shows "Disconnected" once three reconnects in a row have failed', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      renderApp(<BoardScreen />, {
        server: board(
          [],
          [{ on: "GET /requests/events", reply: () => apiErrorResponse(500, "x") }],
        ),
      });
      await screen.findByText("No requests found.");
      expect(screen.getByTestId("board-freshness")).not.toHaveTextContent("Disconnected");
      await act(async () => {
        await vi.advanceTimersByTimeAsync(11_000);
      });
      expect(screen.getByTestId("board-freshness")).toHaveTextContent("Disconnected");
    } finally {
      vi.useRealTimers();
    }
  });

  test("a permanent stream failure shows the strip and Disconnected", async () => {
    renderApp(<BoardScreen />, {
      server: board([], [{ on: "GET /requests/events", reply: () => apiErrorResponse(403, "no") }]),
    });
    expect(await screen.findByTestId("board-stream-banner")).toHaveTextContent(
      "Live updates stopped",
    );
    expect(screen.getByTestId("board-freshness")).toHaveTextContent("Disconnected");
  });
});

test("an event from the stream updates a row in place", async () => {
  renderApp(<BoardScreen />, {
    server: board(
      [{ id: "req-a", state: "building", title: "Streaming" }],
      [
        {
          on: "GET /requests/events",
          reply: sseResponse("state", [
            requestJson({
              id: "req-a",
              state: "done",
              title: "Streaming",
              updatedAt: "2026-09-10T10:00:00Z",
            }),
          ]),
        },
      ],
    ),
  });
  expect(await screen.findByText("Finished (1)")).toBeInTheDocument();
});

test("Refresh is a button named Refresh", async () => {
  renderApp(<BoardScreen />, { server: board([]) });
  await screen.findByText("No requests found.");
  expect(screen.getByRole("heading", { level: 1, name: "Requests" })).toBeInTheDocument();
  await waitFor(() => {
    expect(screen.getByRole("button", { name: "Refresh" })).toBeEnabled();
  });
});
