import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import {
  getCollapsedLanes,
  getStoredBoardView,
  setCollapsedLanes,
  setStoredBoardView,
} from "@/platform/boardPrefs";
import { setOperatorName } from "@/platform/operatorIdentity";
import {
  type FakeRoute,
  apiErrorResponse,
  fixtureResponse,
  json,
  renderApp,
  sseResponse,
} from "@/test/render";
import { requestJson, ticketJson } from "@/test/requestFixtures";

import { BoardScreen } from "./BoardScreen";

type Wire = Record<string, unknown>;

const alive = { state: "alive", last_heartbeat: "2026-09-10T09:50:00Z" };

function server(requests: readonly Wire[], extra: FakeRoute[] = []): FakeRoute[] {
  return [
    { on: "GET /requests", reply: () => json(requests) },
    { on: "GET /requests/events", reply: sseResponse("state") },
    { on: "GET /queue-run", reply: () => json(alive) },
    { on: "GET /stats?since=7d", reply: fixtureResponse("stats.json") },
    ...extra,
  ];
}

/** The contract fixtures, as a real server would answer the home screen. */
function fixtureServer(queueRun: Wire | (() => Response) = alive): FakeRoute[] {
  return [
    { on: "GET /requests", reply: fixtureResponse("requests.json") },
    { on: "GET /requests/events", reply: sseResponse("state") },
    {
      on: "GET /queue-run",
      reply: typeof queueRun === "function" ? queueRun : () => json(queueRun),
    },
    { on: "GET /stats?since=7d", reply: fixtureResponse("stats.json") },
  ];
}

const card = (id: string) => screen.getByTestId(`card-${id}`);
const columnHeadings = () =>
  screen.getAllByRole("heading", { level: 2 }).map((heading) => heading.textContent);

// Storage is one per test file: every test starts from the defaults.
beforeEach(() => {
  setOperatorName("operator");
  setStoredBoardView("board");
  setCollapsedLanes([]);
  // The fixtures and the builders are dated 2026-09-10: "now" is that
  // morning. Only the clock's reading is faked; timers run as usual.
  vi.useFakeTimers({ toFake: ["Date"], now: new Date("2026-09-10T10:00:00Z") });
});
afterEach(() => {
  vi.useRealTimers();
});

describe("the board", () => {
  test("the page is Mission Control, with five columns and their counts", async () => {
    renderApp(<BoardScreen />, { server: fixtureServer() });
    expect(
      await screen.findByRole("heading", { level: 1, name: "Mission Control" }),
    ).toBeInTheDocument();
    expect(await screen.findByRole("heading", { name: "Needs you (6)" })).toBeInTheDocument();
    const board = screen.getByRole("region", { name: "Board" });
    expect(
      within(board)
        .getAllByRole("heading", { level: 2 })
        .map((heading) => heading.textContent),
    ).toEqual(["Drafting (0)", "Needs you (6)", "Building (1)", "PR review (0)", "Done (1)"]);
    // Every request is a card in the list of its column.
    const ids = (column: string) =>
      within(within(board).getByRole("list", { name: column }))
        .queryAllByTestId(/^card-/)
        .map((item) => item.dataset.testid);
    expect(ids("Drafting")).toEqual([]);
    expect(ids("Building")).toEqual(["card-req-building"]);
    expect(ids("Done")).toEqual(["card-req-done"]);
    expect(ids("Needs you").sort()).toEqual([
      "card-req-every-field",
      "card-req-halted",
      "card-req-oracle-review",
      "card-req-plan-review",
      "card-req-quarantined",
      "card-req-spec-review",
    ]);
  });

  test("a card is a link to its request", async () => {
    renderApp(<BoardScreen />, { server: fixtureServer() });
    await screen.findByTestId("card-req-done");
    expect(
      within(card("req-done")).getByRole("link", { name: "Add idempotency keys to checkout" }),
    ).toHaveAttribute("href", "/requests/req-done");
    expect(card("req-done")).toHaveTextContent("req-done");
  });

  test("a building card shows the ticket, the stage and the stalled verdict from the list", async () => {
    const { server: fake } = renderApp(<BoardScreen />, { server: fixtureServer() });
    const building = await screen.findByTestId("card-req-building");
    expect(building).toHaveTextContent("Ticket 2 of 2");
    expect(within(building).getByText("build")).toBeInTheDocument();
    expect(within(building).getByTestId("stalled-chip")).toHaveTextContent("stalled");
    // The card's facts come with the list: no call per card.
    expect(
      fake.requests.map((r) => r.url).filter((url) => url.startsWith("/requests/req-")),
    ).toEqual([]);
    expect(fake.requests.some((r) => r.url.startsWith("/runs"))).toBe(false);
  });

  test("a build's round and a waiting request's queue position are on the card", async () => {
    renderApp(<BoardScreen />, {
      server: server([
        {
          ...requestJson({ id: "req-b", state: "building", title: "Build it" }),
          build: {
            run_id: "run-1",
            ticket: 1,
            tickets: 3,
            stage: "verify",
            round: 2,
            max_rounds: 3,
          },
        },
        {
          ...requestJson({ id: "req-q", state: "submitted", title: "Wait" }),
          queue_position: 2,
          waiting_on: "req-b",
        },
      ]),
    });
    const building = await screen.findByTestId("card-req-b");
    expect(building).toHaveTextContent("Ticket 1 of 3");
    expect(building).toHaveTextContent("Round 2 of 3");
    expect(building).toHaveTextContent("verify");
    expect(within(building).queryByTestId("stalled-chip")).not.toBeInTheDocument();
    expect(within(card("req-q")).getByTestId("kanban-queue")).toHaveTextContent(
      "Queued · position 2 · behind req-b",
    );
    expect(card("req-q")).toHaveTextContent("Submitted");
  });

  test("a quarantined card carries a marker and the first line of the reason; an accepted halt does not", async () => {
    renderApp(<BoardScreen />, { server: fixtureServer() });
    const quarantined = await screen.findByTestId("card-req-quarantined");
    expect(within(quarantined).getByTestId("kanban-alert")).toHaveTextContent(
      /^Quarantined: ticket 1 was quarantined: verify failed after 3 rounds/,
    );
    expect(within(card("req-halted")).queryByTestId("kanban-alert")).not.toBeInTheDocument();
    expect(card("req-halted")).toHaveTextContent("Open its pull request");
    expect(card("req-spec-review")).toHaveTextContent("Review the drafted spec.");
  });

  test("PR review shows each ticket's pull request state; Done links to the pull request", async () => {
    renderApp(<BoardScreen />, {
      server: server([
        requestJson({
          id: "req-pr",
          state: "pr_review",
          title: "In review",
          tickets: [ticketJson({ index: 1, prState: "draft" })],
        }),
        requestJson({
          id: "req-ready",
          state: "pr_review",
          title: "Yours to merge",
          tickets: [ticketJson({ index: 1, prState: "ready" })],
        }),
        requestJson({
          id: "req-d",
          state: "done",
          title: "Shipped",
          tickets: [ticketJson({ index: 1, prState: "merged" })],
        }),
      ]),
    });
    await screen.findByTestId("card-req-pr");
    const board = screen.getByRole("region", { name: "Board" });
    expect(
      within(within(board).getByRole("list", { name: "PR review" })).getByTestId("card-req-pr"),
    ).toHaveTextContent("PR draft");
    // Every PR waits on a human: the request is the operator's.
    expect(
      within(within(board).getByRole("list", { name: "Needs you" })).getByTestId("card-req-ready"),
    ).toHaveTextContent("PR ready for review");
    const pr = within(card("req-d")).getByRole("link", { name: "Ticket 1 PR" });
    expect(pr).toHaveAttribute("href", "https://github.com/acme/app/pull/1");
    expect(pr).toHaveAttribute("rel", "noopener noreferrer");
  });

  test("cancelled requests stay out of Done until Show cancelled is pressed", async () => {
    renderApp(<BoardScreen />, {
      server: server([
        requestJson({ id: "req-d", state: "done", title: "Shipped" }),
        requestJson({ id: "req-c", state: "cancelled", title: "Dropped" }),
      ]),
    });
    await screen.findByTestId("card-req-d");
    expect(screen.queryByTestId("card-req-c")).not.toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Done (1)" })).toBeInTheDocument();
    const toggle = screen.getByRole("button", { name: "Show cancelled (1)" });
    expect(toggle).toHaveAttribute("aria-pressed", "false");
    await userEvent.click(toggle);
    expect(toggle).toHaveAttribute("aria-pressed", "true");
    expect(card("req-c")).toHaveTextContent("Cancelled");
    expect(screen.getByRole("heading", { name: "Done (2)" })).toBeInTheDocument();
  });

  test("Done draws the newest 20 and Show all opens the list of everything finished", async () => {
    const done = Array.from({ length: 22 }, (_, i) =>
      requestJson({
        id: `req-${String(i).padStart(2, "0")}`,
        state: "done",
        title: `Shipped ${i}`,
        updatedAt: `2026-09-10T09:${String(i).padStart(2, "0")}:00Z`,
      }),
    );
    const { location } = renderApp(<BoardScreen />, { server: server(done) });
    await screen.findByTestId("card-req-21");
    expect(screen.getAllByTestId(/^card-/)).toHaveLength(20);
    expect(screen.queryByTestId("card-req-01")).not.toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Done (22)" })).toBeInTheDocument();
    expect(screen.getByText("2 more not shown")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Show all" }));
    expect(await screen.findByText("Finished (22)")).toBeInTheDocument();
    expect(location()).toBe("/?group=finished");
    expect(screen.getByRole("button", { name: "List" })).toHaveAttribute("aria-pressed", "true");
    // Following the link is not choosing List: the next visit opens on the board.
    expect(getStoredBoardView()).toBe("board");
  });
});

describe("project lanes", () => {
  const two = [
    requestJson({ id: "req-a", state: "spec_review", project: "alpha", title: "Alpha spec" }),
    requestJson({ id: "req-b", state: "building", project: "beta", title: "Beta build" }),
  ];

  test("one project has no lane heading", async () => {
    renderApp(<BoardScreen />, {
      server: server([requestJson({ id: "req-a", state: "building", title: "Only" })]),
    });
    await screen.findByTestId("card-req-a");
    expect(screen.queryByRole("region", { name: /^Project / })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { expanded: true })).not.toBeInTheDocument();
  });

  test("two projects get a lane each, with the cards in their own lane", async () => {
    renderApp(<BoardScreen />, { server: server(two) });
    const alpha = await screen.findByRole("region", { name: "Project alpha" });
    const beta = screen.getByRole("region", { name: "Project beta" });
    expect(within(alpha).getByRole("list", { name: "Needs you" })).toContainElement(card("req-a"));
    expect(within(beta).getByRole("list", { name: "Building" })).toContainElement(card("req-b"));
    expect(within(beta).getByRole("list", { name: "Needs you" })).toBeEmptyDOMElement();
    // The lanes say the project: a card does not repeat it.
    expect(card("req-a")).not.toHaveTextContent("alpha");
  });

  test("a lane collapses and expands, and the choice outlives the screen", async () => {
    const first = renderApp(<BoardScreen />, { server: server(two) });
    const alpha = await screen.findByRole("region", { name: "Project alpha" });
    const fold = within(alpha).getByRole("button", { name: "alpha (1)" });
    expect(fold).toHaveAttribute("aria-expanded", "true");
    await userEvent.click(fold);
    expect(fold).toHaveAttribute("aria-expanded", "false");
    expect(within(alpha).queryByRole("list", { name: "Needs you" })).not.toBeInTheDocument();
    expect(screen.getByTestId("card-req-a")).not.toBeVisible();
    expect(card("req-b")).toBeVisible();
    // The column header still counts the folded lane's card.
    expect(screen.getByRole("heading", { name: "Needs you (1)" })).toBeInTheDocument();
    expect(getCollapsedLanes()).toEqual(["alpha"]);
    first.unmount();

    renderApp(<BoardScreen />, { server: server(two) });
    const again = await screen.findByRole("region", { name: "Project alpha" });
    const foldAgain = within(again).getByRole("button", { name: "alpha (1)" });
    expect(foldAgain).toHaveAttribute("aria-expanded", "false");
    await userEvent.click(foldAgain);
    expect(foldAgain).toHaveAttribute("aria-expanded", "true");
    expect(card("req-a")).toBeVisible();
    expect(getCollapsedLanes()).toEqual([]);
  });

  test("a project filter that leaves one lane drops the lane heading and names the project on the card", async () => {
    renderApp(<BoardScreen />, { server: server(two), path: "/?project=alpha" });
    const alphaCard = await screen.findByTestId("card-req-a");
    expect(screen.queryByRole("region", { name: /^Project / })).not.toBeInTheDocument();
    expect(alphaCard).toHaveTextContent("alpha");
    expect(screen.queryByTestId("card-req-b")).not.toBeInTheDocument();
  });
});

describe("deciding from a card", () => {
  const spec = "# Spec\n\nCharge once.\n";
  const listed = requestJson({ id: "req-s", state: "spec_review", title: "Charge once" });
  const detail = { ...listed, spec };

  test("a review card links to the request page with Review, and never offers Approve", async () => {
    const { server: fake } = renderApp(<BoardScreen />, { server: fixtureServer() });
    await screen.findByTestId("card-req-spec-review");
    for (const id of ["req-spec-review", "req-plan-review", "req-oracle-review"]) {
      expect(within(card(id)).getByRole("link", { name: "Review" })).toHaveAttribute(
        "href",
        `/requests/${id}`,
      );
      // Review first, then Request changes: the card's last two controls.
      const controls = [...card(id).querySelectorAll("a, button")].map((el) => el.textContent);
      expect(controls.slice(-2)).toEqual(["Review", "Request changes"]);
    }
    // An approval passes a gate: no card in any column offers one.
    expect(screen.queryByRole("button", { name: /approve/i })).not.toBeInTheDocument();
    expect(screen.getAllByTestId(/^card-/)).toHaveLength(8);
    // Every button the board itself offers can be pressed without an approve call.
    for (const control of within(screen.getByRole("region", { name: "Board" })).getAllByRole(
      "button",
    )) {
      await userEvent.click(control);
      await userEvent.keyboard("{Escape}");
    }
    expect(fake.requests.filter((r) => r.url.endsWith("/approve"))).toEqual([]);
    expect(fake.requests.filter((r) => r.method !== "GET")).toEqual([]);
  });

  test("Request changes opens the request-changes dialog and sends the reason", async () => {
    const { server: fake } = renderApp(<BoardScreen />, {
      server: server(
        [listed],
        [
          { on: "GET /requests/req-s", reply: () => json(detail) },
          {
            on: "POST /requests/req-s/reject",
            reply: () => json({ ...detail, state: "spec_drafting" }),
          },
        ],
      ),
    });
    const specCard = await screen.findByTestId("card-req-s");
    await userEvent.click(within(specCard).getByRole("button", { name: "Request changes" }));
    const dialog = await screen.findByRole("dialog", { name: "Request changes" });
    await userEvent.type(within(dialog).getByLabelText("Reason"), "Name the account.");
    await userEvent.click(within(dialog).getByRole("button", { name: "Request changes" }));
    await waitFor(() => {
      expect(fake.sent("POST /requests/req-s/reject")).toHaveLength(1);
    });
    expect(fake.sent("POST /requests/req-s/reject")[0]?.body).toMatchObject({
      by: "operator",
      reason: "Name the account.",
    });
  });

  test("a detail that cannot be read shows the error, naming the request, and nothing is sent", async () => {
    const { server: fake } = renderApp(<BoardScreen />, {
      server: server(
        [listed],
        [{ on: "GET /requests/req-s", reply: () => apiErrorResponse(500, "disk gone") }],
      ),
    });
    const specCard = await screen.findByTestId("card-req-s");
    await userEvent.click(within(specCard).getByRole("button", { name: "Request changes" }));
    const status = await screen.findByTestId("card-decision");
    expect(await within(status).findByRole("alert")).toHaveTextContent("Request failed (500)");
    expect(status).toHaveTextContent("Request changes: Charge once");
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    // The pressed button is marked, not disabled: it can still hold the focus.
    const pressed = within(specCard).getByRole("button", { name: "Request changes" });
    expect(pressed).toHaveAttribute("aria-disabled", "true");
    expect(pressed).toBeEnabled();
    await userEvent.click(within(status).getByRole("button", { name: "Dismiss" }));
    expect(screen.queryByTestId("card-decision")).not.toBeInTheDocument();
    expect(pressed).toHaveAttribute("aria-disabled", "false");
    expect(fake.requests.filter((r) => r.method === "POST")).toEqual([]);
  });

  test("only review states carry controls: the other cards are the link alone", async () => {
    renderApp(<BoardScreen />, { server: fixtureServer() });
    await screen.findByTestId("card-req-plan-review");
    for (const id of ["req-quarantined", "req-halted", "req-every-field", "req-building"]) {
      expect(within(card(id)).queryByRole("button")).not.toBeInTheDocument();
      expect(within(card(id)).queryByRole("link", { name: "Review" })).not.toBeInTheDocument();
    }
  });

  test("a console that cannot write still links to the review, and offers no write", async () => {
    renderApp(<BoardScreen />, { server: fixtureServer(), config: { writesEnabled: false } });
    const specCard = await screen.findByTestId("card-req-spec-review");
    expect(within(specCard).queryByRole("button")).not.toBeInTheDocument();
    expect(within(specCard).getByRole("link", { name: "Review" })).toHaveAttribute(
      "href",
      "/requests/req-spec-review",
    );
  });
});

describe("health strip", () => {
  const strip = () => screen.findByRole("region", { name: "Factory health" });

  test("a live worker: its slots, what it runs, the queue and the last transition", async () => {
    renderApp(<BoardScreen />, {
      server: fixtureServer({ ...alive, active_requests: ["req-building"], job_slots: 2 }),
    });
    const health = await strip();
    await within(health).findByText("Running", { selector: "span" });
    expect(health).toHaveTextContent("Job slots1 of 2 busy");
    const running = within(health).getByRole("list", { name: "Running now" });
    expect(
      within(running).getByRole("link", { name: "Add idempotency keys to checkout" }),
    ).toHaveAttribute("href", "/requests/req-building");
    expect(running).toHaveTextContent("ticket 2 of 2 · build");
    expect(within(running).getByTestId("stalled-chip")).toBeInTheDocument();
    expect(health).toHaveTextContent("Queued0");
    expect(within(health).getByText("Last transition")).toBeInTheDocument();
  });

  test("a live worker with nothing to do says so, and counts what waits", async () => {
    renderApp(<BoardScreen />, {
      server: server(
        [{ ...requestJson({ id: "req-q", state: "submitted" }), queue_position: 1 }],
        [{ on: "GET /queue-run", reply: () => json({ ...alive, job_slots: 1 }) }],
      ),
    });
    const health = await strip();
    await waitFor(() => {
      expect(health).toHaveTextContent("Job slots0 of 1 busy");
    });
    expect(health).toHaveTextContent("Runningnothing");
    expect(health).toHaveTextContent("Queued1");
  });

  test("a stale worker (the fixture) is named stale, with no slots, nothing running and no queue length", async () => {
    renderApp(<BoardScreen />, { server: fixtureServer(fixtureResponse("queue-run.json")) });
    const health = await strip();
    await within(health).findByText("Stale");
    expect(health).toHaveTextContent("last heartbeat");
    expect(health).not.toHaveTextContent("Job slots");
    // One request is building and nothing advances it: never "Queued 0".
    expect(health).not.toHaveTextContent("Queued");
    expect(within(health).getByTestId("health-need-worker")).toHaveTextContent(
      "1 request, not advancing",
    );
    expect(health).toHaveTextContent("Waiting for a worker1 request, not advancing");
    expect(within(health).queryByRole("list", { name: "Running now" })).not.toBeInTheDocument();
    // The warning strip above the board still says what to do about it.
    expect(screen.getByTestId("worker-down-banner")).toBeInTheDocument();
  });

  test("an absent worker is named not running", async () => {
    renderApp(<BoardScreen />, { server: fixtureServer({ state: "absent" }) });
    const health = await strip();
    await within(health).findByText("Not running");
    expect(health).not.toHaveTextContent("Job slots");
    expect(health).not.toHaveTextContent("Queued");
    expect(within(health).getByTestId("health-need-worker")).toHaveTextContent(
      "1 request, not advancing",
    );
  });

  test("five requests behind a dead worker are counted as waiting for one; none in a job state shows no such fact", async () => {
    const first = renderApp(<BoardScreen />, {
      server: server(
        ["submitted", "spec_drafting", "oracle_drafting", "planning", "building"].map((state) =>
          requestJson({ id: `req-${state}`, state }),
        ),
        [{ on: "GET /queue-run", reply: () => json({ state: "stale" }) }],
      ),
    });
    expect(await screen.findByTestId("health-need-worker")).toHaveTextContent(
      "5 requests, not advancing",
    );
    first.unmount();
    renderApp(<BoardScreen />, {
      server: server(
        [requestJson({ id: "req-r", state: "spec_review" })],
        [{ on: "GET /queue-run", reply: () => json({ state: "stale" }) }],
      ),
    });
    const health = await strip();
    await within(health).findByText("Stale");
    expect(health).not.toHaveTextContent("Waiting for a worker");
    expect(health).not.toHaveTextContent("Queued");
  });

  test("a server with no queue-run route says the worker's state is unknown, and claims no queue", async () => {
    renderApp(<BoardScreen />, {
      server: fixtureServer(() => apiErrorResponse(404, "not found")),
    });
    const health = await strip();
    expect(health).toHaveTextContent("Workerstate unknown");
    expect(health).not.toHaveTextContent("Queued");
    expect(health).not.toHaveTextContent("Waiting for a worker");
    expect(health).toHaveTextContent("In job states1 request");
  });
});

describe("numbers", () => {
  const numbers = () => screen.findByRole("region", { name: "Numbers" });

  test("the fixture's numbers, overall and per project", async () => {
    renderApp(<BoardScreen />, { server: fixtureServer() });
    const region = await numbers();
    const rows = within(region)
      .getAllByTestId("numbers-row")
      .map((row) =>
        within(row)
          .getAllByRole("cell")
          .map((cell) => cell.textContent),
      );
    expect(
      within(region)
        .getAllByRole("rowheader")
        .map((h) => h.textContent),
    ).toEqual(["Overall", "app"]);
    const figures = [
      "3",
      "1/3 (33%)",
      "2/3 (67%)",
      "1",
      "canonical_verify (1)",
      "478.3k tokens · $1.50",
      "$0.75",
    ];
    expect(rows).toEqual([figures, figures]);
    expect(
      within(region)
        .getAllByRole("columnheader")
        .map((h) => h.textContent),
    ).toEqual([
      "Project",
      "Tickets",
      "One-shot",
      "Accepted",
      "Median rounds",
      "Top quarantine check",
      "Spend",
      "Cost / accepted ticket",
    ]);
  });

  const emptyMetrics = {
    tickets: 0,
    one_shot_rate: null,
    accepted_rate: null,
    rounds_to_green: { series: 0, median: 0, p90: 0 },
    quarantined_by: [],
    halted_by: [],
    corrective_builds: { ran: 0, accepted: 0 },
    spend: { tokens: 0, cost_micro_usd: 0, per_accepted_ticket_micro_usd: 0 },
  };
  const report = (project: string) => ({
    project,
    bucket_days: 7,
    overall: emptyMetrics,
    buckets: [],
  });
  const withStats = (stats: () => Response): FakeRoute[] => [
    ...server([requestJson({ id: "req-a", state: "building" })]).filter(
      (route) => route.on !== "GET /stats?since=7d",
    ),
    { on: "GET /stats?since=7d", reply: stats },
  ];

  test("a project with no ticket shows - for its rates, never 0%", async () => {
    renderApp(<BoardScreen />, {
      server: withStats(() => json({ overall: report(""), projects: [report("idle")] })),
    });
    const region = await numbers();
    const idle = within(region).getAllByTestId("numbers-row")[1]!;
    expect(
      within(idle)
        .getAllByRole("cell")
        .map((cell) => cell.textContent),
    ).toEqual(["0", "-", "-", "-", "-", "-", "-"]);
    expect(region).not.toHaveTextContent("%");
  });

  test("an empty data dir says the numbers come with the first finished run", async () => {
    renderApp(<BoardScreen />, {
      server: withStats(() => json({ overall: report(""), projects: [] })),
    });
    const region = await numbers();
    expect(region).toHaveTextContent("No ticket has a finished run yet");
    expect(within(region).queryByRole("table")).not.toBeInTheDocument();
  });

  test("a failed read says so and can be retried", async () => {
    const { server: fake } = renderApp(<BoardScreen />, {
      server: withStats(() => apiErrorResponse(500, "stats broke")),
    });
    const region = await numbers();
    expect(region).toHaveTextContent("The numbers could not be loaded:");
    fake.set("GET /stats?since=7d", fixtureResponse("stats.json"));
    await userEvent.click(within(region).getByRole("button", { name: "Retry" }));
    expect(await within(region).findAllByTestId("numbers-row")).toHaveLength(2);
  });

  test("a server with no stats route shows no numbers section and no error", async () => {
    renderApp(<BoardScreen />, {
      server: withStats(() => apiErrorResponse(404, "not found")),
    });
    await screen.findByTestId("card-req-a");
    await waitFor(() => {
      expect(screen.queryByRole("region", { name: "Numbers" })).not.toBeInTheDocument();
    });
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });
});

describe("activity", () => {
  test("the latest moves across requests, newest first, each linked to its request", async () => {
    const move = (from: string, to: string, at: string, by: string) => ({ from, to, at, by });
    renderApp(<BoardScreen />, {
      server: server([
        {
          ...requestJson({ id: "req-a", state: "spec_review", title: "Alpha" }),
          history: [
            move("submitted", "spec_drafting", "2026-09-10T09:00:00Z", "factoryd"),
            move("spec_drafting", "spec_review", "2026-09-10T09:05:00Z", "factoryd"),
          ],
        },
        {
          ...requestJson({ id: "req-b", state: "planning", title: "Beta" }),
          history: [move("spec_review", "planning", "2026-09-10T09:03:00Z", "kim")],
        },
      ]),
    });
    const region = await screen.findByRole("region", { name: "Activity" });
    const entries = within(region).getAllByTestId("activity-entry");
    expect(entries.map((entry) => entry.textContent)).toEqual([
      expect.stringMatching(/^AlphaDrafting spec → Spec review·by factoryd·/),
      expect.stringMatching(/^BetaSpec review → Planning·by kim·/),
      expect.stringMatching(/^AlphaSubmitted → Drafting spec·by factoryd·/),
    ]);
    expect(within(entries[1]!).getByRole("link", { name: "Beta" })).toHaveAttribute(
      "href",
      "/requests/req-b",
    );
    expect(within(entries[1]!).getByText(/ago$/)).toHaveAttribute(
      "datetime",
      "2026-09-10T09:03:00Z",
    );
  });

  test("the fixture list shows fifteen moves; requests with no history show no section", async () => {
    const first = renderApp(<BoardScreen />, { server: fixtureServer() });
    const region = await screen.findByRole("region", { name: "Activity" });
    expect(within(region).getAllByTestId("activity-entry")).toHaveLength(15);
    first.unmount();
    renderApp(<BoardScreen />, { server: server([requestJson({ id: "req-a", state: "done" })]) });
    await screen.findByTestId("card-req-a");
    expect(screen.queryByRole("region", { name: "Activity" })).not.toBeInTheDocument();
  });
});

describe("Board and List", () => {
  test("List shows the three sections, the choice is remembered, and Board brings the columns back", async () => {
    const first = renderApp(<BoardScreen />, { server: fixtureServer() });
    await screen.findByRole("region", { name: "Board" });
    const view = screen.getByRole("group", { name: "View" });
    expect(within(view).getByRole("button", { name: "Board" })).toHaveAttribute(
      "aria-pressed",
      "true",
    );
    await userEvent.click(within(view).getByRole("button", { name: "List" }));
    expect(screen.queryByRole("region", { name: "Board" })).not.toBeInTheDocument();
    expect(screen.getByText("Needs you (6)")).toBeInTheDocument();
    expect(screen.getByText("Working (1)")).toBeInTheDocument();
    expect(screen.getByText("Finished (1)")).toBeInTheDocument();
    expect(screen.getByRole("table", { name: "Needs you" })).toBeInTheDocument();
    expect(screen.getByTestId("request-req-building")).toBeInTheDocument();
    // The rest of Mission Control stays under the list.
    expect(screen.getByRole("region", { name: "Factory health" })).toBeInTheDocument();
    expect(getStoredBoardView()).toBe("list");
    first.unmount();

    renderApp(<BoardScreen />, { server: fixtureServer() });
    expect(await screen.findByText("Working (1)")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "List" })).toHaveAttribute("aria-pressed", "true");
    await userEvent.click(screen.getByRole("button", { name: "Board" }));
    expect(screen.getByRole("region", { name: "Board" })).toBeInTheDocument();
    expect(screen.queryByText("Working (1)")).not.toBeInTheDocument();
    expect(getStoredBoardView()).toBe("board");
  });

  test("the tab title carries the needs-you count in the board view too", async () => {
    renderApp(<BoardScreen />, { server: fixtureServer() });
    await screen.findByRole("region", { name: "Board" });
    await waitFor(() => {
      expect(document.title).toBe("(6) Buildgate");
    });
  });

  test("the toolbar, the freshness indicator and the warning strips are on the board view", async () => {
    renderApp(<BoardScreen />, {
      server: fixtureServer({ state: "stale", last_heartbeat: "2026-09-10T09:50:00Z" }),
      config: { releasePolicyWarning: "policy denies everything" },
    });
    await screen.findByRole("region", { name: "Board" });
    expect(screen.getByRole("searchbox", { name: "Search id, title, workspace" })).toBeVisible();
    expect(await screen.findByTestId("board-freshness")).toHaveTextContent("Live");
    expect(screen.getByTestId("release-policy-warning-banner")).toBeInTheDocument();
    expect(await screen.findByTestId("worker-down-banner")).toBeInTheDocument();
  });
});

describe("URL filters on the board", () => {
  const mixed = [
    requestJson({ id: "req-r", state: "spec_review", title: "Review me" }),
    requestJson({ id: "req-p", state: "planning", title: "Planning it" }),
    requestJson({ id: "req-b", state: "building", title: "Building it" }),
    requestJson({ id: "req-d", state: "done", title: "Shipped" }),
  ];

  test("?group= narrows the board to that section's columns", async () => {
    renderApp(<BoardScreen />, { server: server(mixed), path: "/?group=working" });
    await screen.findByTestId("card-req-b");
    expect(columnHeadings().slice(0, 3)).toEqual(["Drafting (1)", "Building (1)", "PR review (0)"]);
    expect(screen.queryByRole("heading", { name: /^Needs you/ })).not.toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: /^Done/ })).not.toBeInTheDocument();
    expect(screen.queryByTestId("card-req-r")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Working" })).toHaveAttribute("aria-pressed", "true");
    // The request that waits is out of view: the strip says so, as on the list.
    expect(screen.getByTestId("needs-you-banner")).toBeInTheDocument();
  });

  test("the section chips and the needs-you strip drive the board's columns", async () => {
    const { location } = renderApp(<BoardScreen />, { server: server(mixed) });
    await screen.findByTestId("card-req-b");
    await userEvent.click(screen.getByRole("button", { name: "Finished" }));
    expect(location()).toBe("/?group=finished");
    expect(columnHeadings().slice(0, 1)).toEqual(["Done (1)"]);
    await userEvent.click(within(screen.getByTestId("needs-you-banner")).getByRole("button"));
    expect(location()).toBe("/?group=needs-you");
    expect(columnHeadings().slice(0, 1)).toEqual(["Needs you (1)"]);
    expect(card("req-r")).toBeInTheDocument();
  });

  test("?q= and ?project= filter the cards", async () => {
    renderApp(<BoardScreen />, { server: server(mixed), path: "/?q=req-d" });
    await screen.findByTestId("card-req-d");
    expect(screen.getAllByTestId(/^card-/)).toHaveLength(1);
    expect(screen.getByRole("heading", { name: "Building (0)" })).toBeInTheDocument();
  });

  test("a filter that matches nothing says so", async () => {
    renderApp(<BoardScreen />, { server: server(mixed), path: "/?q=nothing-matches" });
    expect(await screen.findByText("No requests found.")).toBeInTheDocument();
    expect(screen.queryByRole("region", { name: "Board" })).not.toBeInTheDocument();
  });
});

describe("the window on finished work", () => {
  // "Now" is 2026-09-10T10:00Z: three weeks ago is outside 7 days, inside 30.
  const old = "2026-08-20T09:00:00Z";
  const recent = "2026-09-09T09:00:00Z";
  const move = (to: string, at: string) => ({ from: "building", to, at, by: "factoryd" });
  const requests = [
    {
      ...requestJson({ id: "req-wait", state: "spec_review", title: "Old wait", updatedAt: old }),
      entered_at: old,
      history: [move("spec_review", old)],
    },
    requestJson({ id: "req-build", state: "building", title: "Old build", updatedAt: old }),
    {
      ...requestJson({ id: "req-old", state: "done", title: "Old done", updatedAt: old }),
      history: [move("done", old)],
    },
    requestJson({ id: "req-oldc", state: "cancelled", title: "Old cancelled", updatedAt: old }),
    {
      ...requestJson({ id: "req-new", state: "done", title: "New done", updatedAt: recent }),
      history: [move("done", recent)],
    },
  ];
  const statsRoutes: FakeRoute[] = ["", "?since=7d", "?since=30d"].map((query) => ({
    on: `GET /stats${query}`,
    reply: fixtureResponse("stats.json"),
  }));
  const windowed = (path = "/") =>
    renderApp(<BoardScreen />, { server: server(requests, statsRoutes), path });
  const chips = () => screen.getByRole("group", { name: "Finished work from the last" });
  const pressed = () =>
    within(chips())
      .getAllByRole("button")
      .map((chip) => `${chip.textContent}:${chip.getAttribute("aria-pressed")}`);

  test("7 days is the default: old finished work is out, old work in flight stays", async () => {
    const { server: fake, location } = windowed();
    await screen.findByTestId("card-req-new");
    expect(pressed()).toEqual(["7 days:true", "30 days:false", "All:false"]);
    expect(location()).toBe("/");
    expect(screen.queryByTestId("card-req-old")).not.toBeInTheDocument();
    // Three weeks old and still waiting or building: never hidden.
    expect(card("req-wait")).toBeInTheDocument();
    expect(card("req-build")).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Done (1)" })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Needs you (1)" })).toBeInTheDocument();
    // Done says what "All time" would bring back: the old done request. The
    // old cancelled one stays behind Show cancelled either way, so it is not counted.
    expect(screen.getByTestId("older-hidden")).toHaveTextContent("1 older hidden");
    expect(screen.queryByRole("button", { name: /^Show cancelled/ })).not.toBeInTheDocument();
    // Activity is the window's too.
    const entries = within(screen.getByRole("region", { name: "Activity" })).getAllByTestId(
      "activity-entry",
    );
    expect(entries.map((entry) => within(entry).getByRole("link").textContent)).toEqual([
      "New done",
    ]);
    expect(screen.getByRole("region", { name: "Activity" })).toHaveTextContent("Last 7 days");
    // And the numbers: asked for with the window, and headed with it.
    expect(await screen.findByTestId("numbers-window")).toHaveTextContent("Last 7 days");
    expect(fake.requests.map((r) => r.url).filter((url) => url.startsWith("/stats"))).toEqual([
      "/stats?since=7d",
    ]);
  });

  test("the hidden note's control widens the window to all time", async () => {
    const { server: fake, location } = windowed();
    await screen.findByTestId("older-hidden");
    await userEvent.click(
      within(screen.getByTestId("older-hidden")).getByRole("button", { name: "All time" }),
    );
    expect(location()).toBe("/?days=all");
    expect(pressed()).toEqual(["7 days:false", "30 days:false", "All:true"]);
    expect(card("req-old")).toBeInTheDocument();
    expect(screen.queryByTestId("older-hidden")).not.toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Done (2)" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Show cancelled (1)" })).toBeInTheDocument();
    expect(
      within(screen.getByRole("region", { name: "Activity" })).getAllByTestId("activity-entry"),
    ).toHaveLength(3);
    expect(
      await screen.findByText("All time", { selector: "[data-testid='numbers-window']" }),
    ).toBeInTheDocument();
    await waitFor(() => {
      expect(fake.sent("GET /stats")).toHaveLength(1);
    });
  });

  test("each choice is in the URL and asks for the numbers with its own since", async () => {
    const { server: fake, location } = windowed();
    await screen.findByTestId("card-req-new");
    await userEvent.click(within(chips()).getByRole("button", { name: "30 days" }));
    expect(location()).toBe("/?days=30");
    expect(card("req-old")).toBeInTheDocument();
    expect(screen.queryByTestId("older-hidden")).not.toBeInTheDocument();
    await waitFor(() => {
      expect(fake.sent("GET /stats?since=30d")).toHaveLength(1);
    });
    expect(screen.getByTestId("numbers-window")).toHaveTextContent("Last 30 days");
    await userEvent.click(within(chips()).getByRole("button", { name: "All" }));
    expect(location()).toBe("/?days=all");
    await waitFor(() => {
      expect(fake.sent("GET /stats")).toHaveLength(1);
    });
    await userEvent.click(within(chips()).getByRole("button", { name: "7 days" }));
    expect(location()).toBe("/");
    expect(screen.queryByTestId("card-req-old")).not.toBeInTheDocument();
    // Pressing the choice in use keeps it: one of the three is always on.
    await userEvent.click(within(chips()).getByRole("button", { name: "7 days" }));
    expect(pressed()).toEqual(["7 days:true", "30 days:false", "All:false"]);
  });

  test("a link with days= opens on that window, beside the other filters", async () => {
    const { location } = windowed("/?days=30&q=done");
    await screen.findByTestId("card-req-old");
    expect(pressed()).toEqual(["7 days:false", "30 days:true", "All:false"]);
    await userEvent.click(screen.getByRole("button", { name: "Finished" }));
    expect(location()).toBe("/?group=finished&q=done&days=30");
  });

  test("the List view's Finished section follows the window; the other sections do not", async () => {
    setStoredBoardView("list");
    windowed();
    expect(await screen.findByText("Finished (1)")).toBeInTheDocument();
    expect(screen.getByText("Needs you (1)")).toBeInTheDocument();
    expect(screen.getByText("Working (1)")).toBeInTheDocument();
    expect(screen.getByTestId("request-req-wait")).toBeInTheDocument();
    expect(screen.queryByTestId("request-req-old")).not.toBeInTheDocument();
    // The list draws cancelled requests, so both old finished ones are counted here.
    expect(screen.getByTestId("older-hidden")).toHaveTextContent("2 older hidden");
    await userEvent.click(
      within(screen.getByTestId("older-hidden")).getByRole("button", { name: "All time" }),
    );
    expect(await screen.findByText("Finished (3)")).toBeInTheDocument();
    expect(screen.getByTestId("request-req-old")).toBeInTheDocument();
  });

  test("when the window hides every finished request, Done is empty and says so", async () => {
    renderApp(<BoardScreen />, {
      server: server([requestJson({ id: "req-old", state: "done", updatedAt: old })], statsRoutes),
    });
    expect(await screen.findByTestId("older-hidden")).toHaveTextContent("1 older hidden");
    expect(screen.getByRole("heading", { name: "Done (0)" })).toBeInTheDocument();
    expect(screen.queryByTestId("card-req-old")).not.toBeInTheDocument();
  });
});

describe("scroll boxes", () => {
  test("with one project each column is its own focusable scroll box under a fixed header", async () => {
    renderApp(<BoardScreen />, { server: fixtureServer() });
    await screen.findByTestId("card-req-building");
    const needsYou = within(screen.getByRole("region", { name: "Board" })).getByRole("list", {
      name: "Needs you",
    });
    expect(needsYou).toHaveAttribute("tabindex", "0");
    expect(needsYou).toHaveClass("overflow-y-auto");
    // The header is outside the scrolling list.
    expect(needsYou).not.toContainElement(screen.getByRole("heading", { name: /^Needs you/ }));
    expect(screen.getByRole("group", { name: "Board columns" })).toHaveAttribute("tabindex", "0");
    expect(screen.getByRole("list", { name: "Recent activity" })).toHaveAttribute("tabindex", "0");
    expect(await screen.findByRole("group", { name: "Numbers table" })).toHaveAttribute(
      "tabindex",
      "0",
    );
  });

  test("with several projects the lanes are the scroll box", async () => {
    renderApp(<BoardScreen />, {
      server: server([
        requestJson({ id: "req-a", state: "spec_review", project: "alpha" }),
        requestJson({ id: "req-b", state: "building", project: "beta" }),
      ]),
    });
    const lanes = await screen.findByRole("group", { name: "Project lanes" });
    expect(lanes).toHaveAttribute("tabindex", "0");
    expect(lanes).toHaveClass("overflow-y-auto");
    expect(within(lanes).getAllByRole("region")).toHaveLength(2);
    expect(within(lanes).getAllByRole("list", { name: "Needs you" })[0]).not.toHaveAttribute(
      "tabindex",
    );
  });

  test("the List view scrolls in its own focusable box", async () => {
    setStoredBoardView("list");
    renderApp(<BoardScreen />, { server: fixtureServer() });
    const list = await screen.findByRole("group", { name: "Request list" });
    expect(list).toHaveAttribute("tabindex", "0");
    expect(within(list).getByRole("table", { name: "Needs you" })).toBeInTheDocument();
  });
});

describe("groups inside a column", () => {
  const needsYou = () =>
    within(screen.getByRole("region", { name: "Board" })).getByRole("list", { name: "Needs you" });
  const groupCards = (list: HTMLElement, name: string) =>
    within(within(list).getByRole("group", { name }))
      .getAllByTestId(/^card-/)
      .map((item) => item.dataset.testid);
  const groupNames = (list: HTMLElement) =>
    within(list)
      .getAllByRole("group")
      .map((group) => group.getAttribute("aria-label"));

  test("Needs you groups the fixture's cards under counted sub-headings, in order", async () => {
    const listed = JSON.parse(await fixtureResponse("requests.json")().text()) as Wire[];
    renderApp(<BoardScreen />, {
      server: [
        ...fixtureServer().filter((route) => route.on !== "GET /requests"),
        {
          on: "GET /requests",
          reply: () =>
            json([
              ...listed,
              // The fixtures have no pr_review request whose PRs wait on a human.
              requestJson({
                id: "req-pr-ready",
                state: "pr_review",
                project: "app",
                title: "Yours to merge",
                tickets: [ticketJson({ index: 1, prState: "ready" })],
              }),
            ]),
        },
      ],
    });
    await screen.findByTestId("card-req-pr-ready");
    const list = needsYou();
    expect(groupNames(list)).toEqual([
      "Spec review",
      "Oracle review",
      "Plan review",
      "PR ready",
      "Stuck",
    ]);
    expect(
      within(list)
        .getAllByRole("heading", { level: 3 })
        .map((heading) => heading.textContent),
    ).toEqual([
      "Spec review (1)",
      "Oracle review (1)",
      "Plan review (1)",
      "PR ready (1)",
      "Stuck (3)",
    ]);
    expect(groupCards(list, "Spec review")).toEqual(["card-req-spec-review"]);
    expect(groupCards(list, "Oracle review")).toEqual(["card-req-oracle-review"]);
    expect(groupCards(list, "Plan review")).toEqual(["card-req-plan-review"]);
    expect(groupCards(list, "PR ready")).toEqual(["card-req-pr-ready"]);
    expect(groupCards(list, "Stuck").sort()).toEqual([
      "card-req-every-field",
      "card-req-halted",
      "card-req-quarantined",
    ]);
    // One count per column, over all its groups; the tab title's count is the same.
    expect(screen.getByRole("heading", { level: 2, name: "Needs you (7)" })).toBeInTheDocument();
    await waitFor(() => {
      expect(document.title).toBe("(7) Buildgate");
    });
    // Building and Done have no groups.
    const building = within(screen.getByRole("region", { name: "Board" })).getByRole("list", {
      name: "Building",
    });
    expect(within(building).queryByRole("group")).not.toBeInTheDocument();
  });

  test("a group with no card is not drawn, and the longest wait leads its group", async () => {
    renderApp(<BoardScreen />, {
      server: server([
        requestJson({ id: "req-late", state: "plan_review", waitingSince: "2026-09-10T09:30:00Z" }),
        requestJson({
          id: "req-early",
          state: "plan_review",
          waitingSince: "2026-09-10T08:00:00Z",
        }),
        requestJson({ id: "req-h", state: "halted" }),
      ]),
    });
    await screen.findByTestId("card-req-h");
    expect(groupNames(needsYou())).toEqual(["Plan review", "Stuck"]);
    expect(groupCards(needsYou(), "Plan review")).toEqual(["card-req-early", "card-req-late"]);
    expect(
      within(needsYou()).queryByRole("group", { name: "Spec review" }),
    ).not.toBeInTheDocument();
  });

  test("Drafting groups by stage, the running job before the queue", async () => {
    renderApp(<BoardScreen />, {
      server: server([
        { ...requestJson({ id: "req-q2", state: "submitted" }), queue_position: 2 },
        { ...requestJson({ id: "req-plan", state: "planning" }) },
        { ...requestJson({ id: "req-q1", state: "spec_drafting" }), queue_position: 1 },
        requestJson({ id: "req-run", state: "oracle_drafting" }),
      ]),
    });
    await screen.findByTestId("card-req-plan");
    const drafting = within(screen.getByRole("region", { name: "Board" })).getByRole("list", {
      name: "Drafting",
    });
    expect(groupNames(drafting)).toEqual(["Spec and oracles", "Planning"]);
    expect(groupCards(drafting, "Spec and oracles")).toEqual([
      "card-req-run",
      "card-req-q1",
      "card-req-q2",
    ]);
    expect(groupCards(drafting, "Planning")).toEqual(["card-req-plan"]);
    expect(screen.getByRole("heading", { level: 2, name: "Drafting (4)" })).toBeInTheDocument();
  });

  test("with project lanes each lane's cell has its own groups, under the lane's heading level", async () => {
    renderApp(<BoardScreen />, {
      server: server([
        requestJson({ id: "req-a1", state: "spec_review", project: "alpha" }),
        requestJson({ id: "req-a2", state: "quarantined", project: "alpha" }),
        requestJson({ id: "req-b1", state: "plan_review", project: "beta" }),
      ]),
    });
    const alpha = await screen.findByRole("region", { name: "Project alpha" });
    const beta = screen.getByRole("region", { name: "Project beta" });
    const cellOf = (lane: HTMLElement) => within(lane).getByRole("list", { name: "Needs you" });
    expect(groupNames(cellOf(alpha))).toEqual(["Spec review", "Stuck"]);
    expect(groupCards(cellOf(alpha), "Stuck")).toEqual(["card-req-a2"]);
    expect(groupNames(cellOf(beta))).toEqual(["Plan review"]);
    expect(within(beta).getByRole("heading", { level: 4, name: "Plan review (1)" })).toBeVisible();
    expect(screen.getByRole("heading", { level: 2, name: "Needs you (3)" })).toBeInTheDocument();
    // Folding a lane still takes its groups out of view.
    await userEvent.click(within(alpha).getByRole("button", { name: "alpha (2)" }));
    expect(within(alpha).queryByRole("group")).not.toBeInTheDocument();
    expect(groupNames(cellOf(beta))).toEqual(["Plan review"]);
  });
});

describe("Request changes from a card, while the board keeps refreshing", () => {
  const spec = "# Spec\n\nCharge once.\n";
  const listed = requestJson({ id: "req-s", state: "spec_review", title: "Charge once" });

  test("a refetch and a stream event while the dialog is open leave it, and the typed reason, in place", async () => {
    let version = 1;
    const { server: fake, queryClient } = renderApp(<BoardScreen />, {
      server: server(
        [listed],
        [
          {
            on: "GET /requests/req-s",
            reply: () =>
              json({ ...listed, spec, updated_at: `2026-09-10T09:0${4 + version++}:00Z` }),
          },
          {
            on: "POST /requests/req-s/reject",
            reply: () => json({ ...listed, state: "spec_drafting" }),
          },
        ],
      ),
    });
    const specCard = await screen.findByTestId("card-req-s");
    await userEvent.click(within(specCard).getByRole("button", { name: "Request changes" }));
    const dialog = await screen.findByRole("dialog", { name: "Request changes" });
    await userEvent.type(within(dialog).getByLabelText("Reason"), "Name the account.");
    // What a window refocus or an event for this request does: every query is read again.
    await act(async () => {
      await queryClient.invalidateQueries();
    });
    await waitFor(() => {
      expect(fake.sent("GET /requests/req-s").length).toBeGreaterThan(1);
    });
    expect(screen.getByRole("dialog", { name: "Request changes" })).toBe(dialog);
    expect(within(dialog).getByLabelText("Reason")).toHaveValue("Name the account.");
    await userEvent.click(within(dialog).getByRole("button", { name: "Request changes" }));
    await waitFor(() => {
      expect(fake.sent("POST /requests/req-s/reject")).toHaveLength(1);
    });
    expect(fake.sent("POST /requests/req-s/reject")[0]?.body).toMatchObject({
      reason: "Name the account.",
    });
  });

  test("the request is being read between the press and the dialog", async () => {
    let release: (response: Response) => void = () => undefined;
    renderApp(<BoardScreen />, {
      server: server(
        [listed],
        [
          {
            on: "GET /requests/req-s",
            reply: () => new Promise<Response>((resolve) => (release = resolve)),
          },
        ],
      ),
    });
    const specCard = await screen.findByTestId("card-req-s");
    await userEvent.click(within(specCard).getByRole("button", { name: "Request changes" }));
    expect(
      await within(screen.getByTestId("card-decision")).findByRole("status", {
        name: "Loading the request",
      }),
    ).toBeInTheDocument();
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    release(json({ ...listed, spec }));
    expect(await screen.findByRole("dialog", { name: "Request changes" })).toBeInTheDocument();
  });

  test("a request that has left the card's stage gets a notice, not a rejection of the next stage", async () => {
    const { server: fake } = renderApp(<BoardScreen />, {
      server: server(
        [listed],
        [
          {
            on: "GET /requests/req-s",
            reply: () => json({ ...listed, spec, state: "plan_review" }),
          },
        ],
      ),
    });
    const specCard = await screen.findByTestId("card-req-s");
    await userEvent.click(within(specCard).getByRole("button", { name: "Request changes" }));
    expect(await screen.findByTestId("card-moved-on")).toHaveTextContent(
      "This request has moved on to Plan review. Nothing was sent.",
    );
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    await userEvent.click(
      within(screen.getByTestId("card-decision")).getByRole("button", { name: "Dismiss" }),
    );
    expect(screen.queryByTestId("card-moved-on")).not.toBeInTheDocument();
    expect(fake.requests.filter((r) => r.method !== "GET")).toEqual([]);
  });
});

test("a folded lane says how many of its cards wait on the operator", async () => {
  renderApp(<BoardScreen />, {
    server: server([
      requestJson({ id: "req-a1", state: "spec_review", project: "alpha" }),
      requestJson({ id: "req-a2", state: "halted", project: "alpha" }),
      requestJson({ id: "req-a3", state: "building", project: "alpha" }),
      requestJson({ id: "req-b1", state: "building", project: "beta" }),
    ]),
  });
  const alpha = await screen.findByRole("region", { name: "Project alpha" });
  const beta = screen.getByRole("region", { name: "Project beta" });
  // Open, the cards say it themselves.
  expect(within(alpha).queryByTestId("lane-needs-you")).not.toBeInTheDocument();
  await userEvent.click(within(alpha).getByRole("button", { name: "alpha (3)" }));
  expect(within(alpha).getByTestId("lane-needs-you")).toHaveTextContent("2 need you");
  // The button keeps its name: the count is beside it, not in it.
  expect(within(alpha).getByRole("button", { name: "alpha (3)" })).toHaveAttribute(
    "aria-expanded",
    "false",
  );
  await userEvent.click(within(beta).getByRole("button", { name: "beta (1)" }));
  expect(within(beta).queryByTestId("lane-needs-you")).not.toBeInTheDocument();
});

describe("numbers and activity follow the project filter, not the search box", () => {
  const move = (at: string) => ({ from: "building", to: "done", at, by: "factoryd" });
  const requests = [
    {
      ...requestJson({ id: "req-a", state: "done", project: "alpha", title: "Alpha done" }),
      history: [move("2026-09-10T09:00:00Z")],
    },
    {
      ...requestJson({ id: "req-b", state: "done", project: "beta", title: "Beta done" }),
      history: [move("2026-09-10T09:30:00Z")],
    },
  ];
  const metrics = (tickets: number) => ({
    tickets,
    one_shot: tickets,
    one_shot_rate: 1,
    accepted: tickets,
    accepted_rate: 1,
    rounds_to_green: { series: tickets, median: 1, p90: 1 },
    quarantined_by: [],
    halted_by: [],
    corrective_builds: { ran: 0, accepted: 0 },
    spend: { tokens: 0, cost_micro_usd: 0, per_accepted_ticket_micro_usd: 0 },
  });
  const report = (project: string, tickets: number) => ({
    project,
    bucket_days: 7,
    overall: metrics(tickets),
    buckets: [],
  });
  const routes = (): FakeRoute[] => [
    ...server(requests).filter((route) => route.on !== "GET /stats?since=7d"),
    {
      on: "GET /stats?since=7d",
      reply: () =>
        json({ overall: report("", 5), projects: [report("alpha", 2), report("beta", 3)] }),
    },
  ];
  const rowLabels = async () =>
    within(await screen.findByRole("region", { name: "Numbers" }))
      .getAllByRole("rowheader")
      .map((header) => header.textContent);
  const activityTitles = () =>
    within(screen.getByRole("region", { name: "Activity" }))
      .getAllByTestId("activity-entry")
      .map((entry) => within(entry).getByRole("link").textContent);

  test("no project chosen: the overall row, every project, every move", async () => {
    renderApp(<BoardScreen />, { server: routes() });
    expect(await rowLabels()).toEqual(["Overall", "alpha", "beta"]);
    expect(activityTitles()).toEqual(["Beta done", "Alpha done"]);
    expect(screen.getByTestId("numbers-window")).toHaveTextContent(/^Last 7 days$/);
  });

  test("a project chosen: its row and its moves only, and the headings say so", async () => {
    const { server: fake } = renderApp(<BoardScreen />, { server: routes() });
    await rowLabels();
    await userEvent.click(
      within(screen.getByRole("group", { name: "Project" })).getByRole("button", { name: "beta" }),
    );
    expect(await rowLabels()).toEqual(["beta"]);
    expect(activityTitles()).toEqual(["Beta done"]);
    expect(screen.getByTestId("numbers-window")).toHaveTextContent("Last 7 days · beta");
    expect(screen.getByRole("region", { name: "Activity" })).toHaveTextContent(
      "Last 7 days · beta",
    );
    // The same read serves every project choice.
    expect(fake.sent("GET /stats?since=7d")).toHaveLength(1);
  });

  test("the search box narrows the cards and leaves both panels alone", async () => {
    renderApp(<BoardScreen />, { server: routes(), path: "/?q=req-a" });
    await screen.findByTestId("card-req-a");
    expect(screen.queryByTestId("card-req-b")).not.toBeInTheDocument();
    expect(await rowLabels()).toEqual(["Overall", "alpha", "beta"]);
    expect(activityTitles()).toEqual(["Beta done", "Alpha done"]);
  });
});

describe("a long Needs you column", () => {
  const board = () => screen.getByRole("region", { name: "Board" });
  const needsYou = () => within(board()).getByRole("list", { name: "Needs you" });
  const chipRow = () => screen.getByRole("group", { name: "Needs you groups" });
  const chipStates = () =>
    within(chipRow())
      .getAllByRole("button")
      .map((chip) => `${chip.textContent}:${chip.getAttribute("aria-pressed")}`);
  const shownCards = () =>
    within(needsYou())
      .getAllByTestId(/^card-/)
      .map((item) => item.dataset.testid);
  const tokens = (element: Element) => element.className.split(/\s+/);
  const many = [
    ...Array.from({ length: 5 }, (_, i) =>
      requestJson({
        id: `req-spec-${i}`,
        state: "spec_review",
        title: `Spec ${i}`,
        waitingSince: `2026-09-10T0${i}:00:00Z`,
      }),
    ),
    ...Array.from({ length: 2 }, (_, i) =>
      requestJson({ id: `req-plan-${i}`, state: "plan_review", title: `Plan ${i}` }),
    ),
    {
      ...requestJson({ id: "req-stuck", state: "quarantined", title: "Stuck one" }),
      error: "verify failed after 3 rounds",
    },
    requestJson({ id: "req-build", state: "building", title: "Building" }),
  ];

  test("cards in Needs you and Drafting are compact; the others are full", async () => {
    renderApp(<BoardScreen />, {
      server: server([...many, requestJson({ id: "req-draft", state: "planning" })]),
    });
    await screen.findByTestId("card-req-build");
    expect(card("req-spec-0")).toHaveAttribute("data-density", "compact");
    expect(card("req-draft")).toHaveAttribute("data-density", "compact");
    expect(card("req-build")).toHaveAttribute("data-density", "full");
    // The title is one line, whole in the link's name and tooltip.
    const title = within(card("req-spec-0")).getByRole("link", { name: "Spec 0" });
    expect(title).toHaveClass("truncate");
    expect(title).toHaveAttribute("title", "Spec 0");
    // The stage and the age stay; a stuck card keeps its red marker.
    expect(card("req-spec-0")).toHaveTextContent("Spec review");
    expect(tokens(within(card("req-spec-0")).getByText(/^for /))).not.toContain("sr-only");
    const alert = within(card("req-stuck")).getByTestId("kanban-alert");
    expect(tokens(alert)).not.toContain("sr-only");
    expect(alert).toHaveTextContent(/^Quarantined/);
    // Its reason, the id and the sentence of what is asked open with the card.
    for (const folded of [
      within(alert).getByText(": verify failed after 3 rounds"),
      within(card("req-spec-0")).getByText("Review the drafted spec."),
      within(card("req-spec-0")).getByTitle("req-spec-0").parentElement!,
    ]) {
      // Hidden from the eye only: a screen reader reading the page still
      // gets it, which `display: none` or `visibility: hidden` would prevent.
      expect(tokens(folded)).toContain("sr-only");
      for (const removed of ["hidden", "invisible"]) expect(tokens(folded)).not.toContain(removed);
      expect(tokens(folded)).toContain("group-focus-within/card:not-sr-only");
      expect(tokens(folded)).toContain("group-hover/card:not-sr-only");
      // No hover, or a finger for a pointer (a phone, a touch laptop): the full card.
      expect(tokens(folded)).toContain("[@media(hover:none)]:not-sr-only");
      expect(tokens(folded)).toContain("[@media(any-pointer:coarse)]:not-sr-only");
    }
    // A full card folds nothing.
    expect(card("req-build").querySelector(".sr-only")).toBeNull();
  });

  test("a compact card's controls stay in the tab order, and focus opens the card", async () => {
    renderApp(<BoardScreen />, { server: server(many) });
    const first = await screen.findByTestId("card-req-spec-0");
    const controls = within(first).getByTestId("kanban-controls");
    // Clipped to no height, never taken out of the layout: a hidden control cannot be tabbed to.
    expect(controls).toHaveClass("max-h-0", "overflow-hidden");
    // Any one of these on the controls, or on anything around them inside the
    // card, would take them out of the tab order.
    for (let node: HTMLElement | null = controls; node !== null && first.contains(node);) {
      for (const removed of ["hidden", "invisible", "sr-only"]) {
        expect(tokens(node)).not.toContain(removed);
      }
      expect(node).not.toHaveAttribute("hidden");
      node = node.parentElement;
    }
    expect(tokens(controls)).toContain("[@media(any-pointer:coarse)]:max-h-none");
    expect(controls.className).toContain("group-focus-within/card:max-h-40");
    expect(controls.className).toContain("group-hover/card:max-h-40");
    expect(controls.className).toContain("[@media(hover:none)]:max-h-none");
    expect(first).toHaveClass("group/card");
    const title = within(first).getByRole("link", { name: "Spec 0" });
    title.focus();
    await userEvent.tab();
    expect(within(first).getByRole("link", { name: "Review" })).toHaveFocus();
    await userEvent.tab();
    expect(within(first).getByRole("button", { name: "Request changes" })).toHaveFocus();
    // The controls are inside the card, so holding the focus is `:focus-within` on it.
    expect(first).toContainElement(document.activeElement as HTMLElement);
  });

  test("the chips carry each group's count, narrow the column to one group and give it back", async () => {
    renderApp(<BoardScreen />, { server: server(many) });
    await screen.findByTestId("card-req-stuck");
    expect(chipStates()).toEqual(["All:true", "Spec 5:false", "Plan 2:false", "Stuck 1:false"]);
    expect(
      within(needsYou())
        .getAllByRole("group")
        .map((group) => group.getAttribute("aria-label")),
    ).toEqual(["Spec review", "Plan review", "Stuck"]);

    await userEvent.click(within(chipRow()).getByRole("button", { name: "Spec 5" }));
    expect(chipStates()).toEqual(["All:false", "Spec 5:true", "Plan 2:false", "Stuck 1:false"]);
    // Only that group, and all of it.
    expect(shownCards()).toEqual([
      "card-req-spec-0",
      "card-req-spec-1",
      "card-req-spec-2",
      "card-req-spec-3",
      "card-req-spec-4",
    ]);
    expect(within(needsYou()).queryByRole("button", { name: /more$/ })).not.toBeInTheDocument();
    // The column's count is still the column's.
    expect(screen.getByRole("heading", { level: 2, name: "Needs you (8)" })).toBeInTheDocument();

    // Pressing it again shows every group; so does All.
    await userEvent.click(within(chipRow()).getByRole("button", { name: "Spec 5" }));
    expect(chipStates()[0]).toBe("All:true");
    expect(within(needsYou()).getAllByRole("group")).toHaveLength(3);
    await userEvent.click(within(chipRow()).getByRole("button", { name: "Stuck 1" }));
    expect(shownCards()).toEqual(["card-req-stuck"]);
    await userEvent.click(within(chipRow()).getByRole("button", { name: "All" }));
    expect(within(needsYou()).getAllByRole("group")).toHaveLength(3);
    // View state for the visit: nothing in the URL.
  });

  test("the chip is not in the URL, and one group needs no chips", async () => {
    const { location } = renderApp(<BoardScreen />, { server: server(many) });
    await screen.findByTestId("card-req-stuck");
    await userEvent.click(within(chipRow()).getByRole("button", { name: "Plan 2" }));
    expect(location()).toBe("/");
  });

  test("a single group draws no chips", async () => {
    renderApp(<BoardScreen />, {
      server: server([requestJson({ id: "req-a", state: "spec_review" })]),
    });
    await screen.findByTestId("card-req-a");
    expect(screen.queryByRole("group", { name: "Needs you groups" })).not.toBeInTheDocument();
  });

  test("a group shows its oldest three, +N more opens the rest and Show fewer folds them", async () => {
    renderApp(<BoardScreen />, { server: server(many) });
    await screen.findByTestId("card-req-stuck");
    const spec = () => within(needsYou()).getByRole("group", { name: "Spec review" });
    const specCards = () =>
      within(spec())
        .getAllByTestId(/^card-/)
        .map((item) => item.dataset.testid);
    expect(specCards()).toEqual(["card-req-spec-0", "card-req-spec-1", "card-req-spec-2"]);
    // The heading's count is the group's real size.
    expect(within(spec()).getByRole("heading", { name: "Spec review (5)" })).toBeInTheDocument();
    const more = within(spec()).getByRole("button", { name: "+2 more" });
    expect(more).toHaveAttribute("aria-expanded", "false");
    // A group that fits has no such button.
    expect(
      within(within(needsYou()).getByRole("group", { name: "Plan review" })).queryByRole("button", {
        name: /more$/,
      }),
    ).not.toBeInTheDocument();
    await userEvent.click(more);
    expect(specCards()).toHaveLength(5);
    expect(specCards().slice(3)).toEqual(["card-req-spec-3", "card-req-spec-4"]);
    const fewer = within(spec()).getByRole("button", { name: "Show fewer" });
    expect(fewer).toHaveAttribute("aria-expanded", "true");
    await userEvent.click(fewer);
    expect(specCards()).toHaveLength(3);
    expect(within(spec()).getByRole("button", { name: "+2 more" })).toBeInTheDocument();
  });

  test("an old request that waits is reachable under the 7-day window: in its group or behind +N more", async () => {
    const old = Array.from({ length: 4 }, (_, i) =>
      requestJson({
        id: `req-old-${i}`,
        state: "halted",
        updatedAt: "2026-07-01T09:00:00Z",
        enteredAt: `2026-07-0${i + 1}T09:00:00Z`,
      }),
    );
    renderApp(<BoardScreen />, { server: server(old) });
    await screen.findByTestId("card-req-old-0");
    expect(screen.getByRole("heading", { level: 2, name: "Needs you (4)" })).toBeInTheDocument();
    expect(screen.queryByTestId("older-hidden")).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "+1 more" }));
    expect(card("req-old-3")).toBeInTheDocument();
  });

  test("an empty column is a narrow strip with its heading and count; a column with a card is not", async () => {
    renderApp(<BoardScreen />, { server: server(many) });
    await screen.findByTestId("card-req-stuck");
    const narrow = (column: string) => screen.getByTestId(`column-${column}`);
    for (const column of ["drafting", "prReview", "done"]) {
      expect(narrow(column)).toHaveAttribute("data-narrow", "true");
      expect(narrow(column)).toHaveClass("w-28", "shrink-0");
    }
    for (const column of ["needsYou", "building"]) {
      expect(narrow(column)).not.toHaveAttribute("data-narrow");
      expect(narrow(column)).toHaveClass("flex-1");
    }
    // Order and headings are kept.
    expect(
      within(board())
        .getAllByRole("heading", { level: 2 })
        .map((heading) => heading.textContent),
    ).toEqual(["Drafting (0)", "Needs you (8)", "Building (1)", "PR review (0)", "Done (0)"]);
    // The cells under them are as wide as their headers.
    expect(within(board()).getByRole("list", { name: "Done" })).toHaveClass("w-28");
    expect(needsYou()).toHaveClass("flex-1");
  });

  test("with lanes a column is narrow only when it is empty in every lane", async () => {
    renderApp(<BoardScreen />, {
      server: server([
        requestJson({ id: "req-a", state: "spec_review", project: "alpha" }),
        requestJson({ id: "req-b", state: "building", project: "beta" }),
      ]),
    });
    const alpha = await screen.findByRole("region", { name: "Project alpha" });
    // Building is empty in alpha and holds a card in beta: full width in both lanes.
    expect(within(alpha).getByRole("list", { name: "Building" })).toHaveClass("flex-1");
    expect(within(alpha).getByRole("list", { name: "Done" })).toHaveClass("w-28");
    expect(screen.getByTestId("column-building")).not.toHaveAttribute("data-narrow");
  });

  test("Needs you goes two across when few columns share the width, and one across on a full board", async () => {
    const first = renderApp(<BoardScreen />, { server: server(many) });
    await screen.findByTestId("card-req-stuck");
    // Needs you and Building share the board: half each.
    const lists = () =>
      within(needsYou())
        .getAllByRole("group")
        .map((group) => within(group).getByRole("list").dataset.across);
    expect(lists()).toEqual(["2", "2", "2"]);
    expect(
      within(within(needsYou()).getByRole("group", { name: "Spec review" })).getByRole("list"),
    ).toHaveClass("xl:grid", "xl:grid-cols-2");
    // Only on a wide screen: below it the cards stay one across.
    const list = within(within(needsYou()).getByRole("group", { name: "Spec review" })).getByRole(
      "list",
    );
    expect(tokens(list)).toContain("flex-col");
    expect(tokens(list)).not.toContain("grid-cols-2");
    first.unmount();

    renderApp(<BoardScreen />, {
      server: server([
        ...many,
        requestJson({ id: "req-d", state: "planning" }),
        requestJson({
          id: "req-p",
          state: "pr_review",
          tickets: [ticketJson({ index: 1, prState: "draft" })],
        }),
      ]),
    });
    await screen.findByTestId("card-req-p");
    expect(lists()).toEqual(["1", "1", "1"]);
    expect(
      within(within(needsYou()).getByRole("group", { name: "Spec review" })).getByRole("list"),
    ).not.toHaveClass("xl:grid-cols-2");
  });

  test("a chip whose group empties is forgotten: the group coming back does not narrow the column again", async () => {
    const stuck = many.find((r) => r.id === "req-stuck")!;
    const { server: fake } = renderApp(<BoardScreen />, { server: server(many) });
    await screen.findByTestId("card-req-stuck");
    await userEvent.click(within(chipRow()).getByRole("button", { name: "Stuck 1" }));
    expect(shownCards()).toEqual(["card-req-stuck"]);
    // The stuck request is retried and leaves Needs you.
    fake.set("GET /requests", () => json(many.filter((r) => r !== stuck)));
    await userEvent.click(screen.getByRole("button", { name: "Refresh" }));
    await waitFor(() => {
      expect(chipStates()).toEqual(["All:true", "Spec 5:false", "Plan 2:false"]);
    });
    expect(within(needsYou()).getAllByRole("group")).toHaveLength(2);
    // Another request gets stuck later: every group is still shown.
    fake.set("GET /requests", () => json(many));
    await userEvent.click(screen.getByRole("button", { name: "Refresh" }));
    await waitFor(() => {
      expect(chipStates()).toEqual(["All:true", "Spec 5:false", "Plan 2:false", "Stuck 1:false"]);
    });
    expect(within(needsYou()).getAllByRole("group")).toHaveLength(3);
  });

  test("the tab title's count is every request that waits, with a chip pressed and with groups folded to three", async () => {
    renderApp(<BoardScreen />, { server: server(many) });
    await screen.findByTestId("card-req-stuck");
    // Eight wait; five are drawn before "+2 more" is opened, plus the two plans and the stuck one.
    expect(shownCards()).toHaveLength(6);
    await waitFor(() => {
      expect(document.title).toBe("(8) Buildgate");
    });
    await userEvent.click(within(chipRow()).getByRole("button", { name: "Plan 2" }));
    expect(shownCards()).toHaveLength(2);
    expect(document.title).toBe("(8) Buildgate");
    expect(screen.getByRole("heading", { level: 2, name: "Needs you (8)" })).toBeInTheDocument();
  });

  test("Needs you links to Triage", async () => {
    renderApp(<BoardScreen />, { server: server(many) });
    await screen.findByTestId("card-req-stuck");
    expect(
      within(screen.getByTestId("column-needsYou")).getByRole("link", { name: "Open in Triage" }),
    ).toHaveAttribute("href", "/triage");
    expect(screen.getAllByRole("link", { name: "Open in Triage" })).toHaveLength(1);
  });
});

describe("an open Request changes dialog and a list that keeps changing", () => {
  const spec = "# Spec\n\nCharge once.\n";
  const listed = requestJson({ id: "req-s", state: "spec_review", title: "Charge once" });
  const detail = { ...listed, spec };
  const open = async (requests: readonly Wire[] = [listed]) => {
    const rendered = renderApp(<BoardScreen />, {
      server: server(requests, [
        { on: "GET /requests/req-s", reply: () => json(detail) },
        {
          on: "POST /requests/req-s/reject",
          reply: () => json({ ...detail, state: "spec_drafting" }),
        },
      ]),
    });
    const specCard = await screen.findByTestId("card-req-s");
    await userEvent.click(within(specCard).getByRole("button", { name: "Request changes" }));
    const dialog = await screen.findByRole("dialog", { name: "Request changes" });
    await userEvent.type(within(dialog).getByLabelText("Reason"), "Name the account.");
    return { ...rendered, dialog };
  };
  /** The list's next answer, as the poll would bring it (the page behind a dialog takes no click). */
  const listBecomes = async (
    rendered: Pick<Awaited<ReturnType<typeof open>>, "server" | "queryClient">,
    next: readonly Wire[],
  ) => {
    rendered.server.set("GET /requests", () => json(next));
    await act(async () => {
      await rendered.queryClient.invalidateQueries();
    });
  };
  const send = (dialog: HTMLElement) =>
    within(dialog).getByRole("button", { name: "Request changes" });

  test("nothing changes: the rejection is sent, and the dialog closes", async () => {
    const rendered = await open();
    const { server: fake, dialog } = rendered;
    expect(within(dialog).queryByTestId("flow-blocked")).not.toBeInTheDocument();
    expect(send(dialog)).toBeEnabled();
    await userEvent.click(send(dialog));
    await waitFor(() => {
      expect(fake.sent("POST /requests/req-s/reject")).toHaveLength(1);
    });
    await waitFor(() => {
      expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    });
  });

  test("the request moves to another stage while the dialog is open: nothing can be sent, the text stays", async () => {
    const rendered = await open();
    const { server: fake, dialog } = rendered;
    await listBecomes(rendered, [
      { ...listed, state: "plan_review", entered_at: "2026-09-10T09:30:00Z" },
    ]);
    expect(await within(dialog).findByTestId("flow-blocked")).toHaveTextContent(
      "This request has moved on to Plan review. Nothing can be sent from this dialog.",
    );
    expect(send(dialog)).toBeDisabled();
    expect(within(dialog).getByLabelText("Reason")).toHaveValue("Name the account.");
    await userEvent.click(send(dialog));
    expect(fake.requests.filter((r) => r.method !== "GET")).toEqual([]);
    await userEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  test("the request is redrafted back into the same state while the dialog is open: nothing can be sent", async () => {
    const rendered = await open();
    const { server: fake, dialog } = rendered;
    // Rejected elsewhere and drafted again: spec_review once more, entered later.
    await listBecomes(rendered, [
      { ...listed, entered_at: "2026-09-10T09:40:00Z", updated_at: "2026-09-10T09:40:00Z" },
    ]);
    expect(await within(dialog).findByTestId("flow-blocked")).toHaveTextContent(
      "it was redrafted and is in Spec review again",
    );
    expect(send(dialog)).toBeDisabled();
    expect(within(dialog).getByLabelText("Reason")).toHaveValue("Name the account.");
    expect(fake.requests.filter((r) => r.method !== "GET")).toEqual([]);
  });

  test("an update that leaves the stage as it was does not block the dialog", async () => {
    const rendered = await open();
    const { server: fake, dialog } = rendered;
    await listBecomes(rendered, [
      { ...listed, updated_at: "2026-09-10T09:45:00Z", title: "Charge once" },
    ]);
    await waitFor(() => {
      expect(fake.sent("GET /requests").length).toBeGreaterThan(1);
    });
    expect(within(dialog).queryByTestId("flow-blocked")).not.toBeInTheDocument();
    expect(send(dialog)).toBeEnabled();
  });

  test("lanes appearing and disappearing under the dialog leave it, and the typed text, in place", async () => {
    const rendered = await open();
    const { server: fake, dialog } = rendered;
    expect(screen.queryByRole("region", { name: /^Project / })).not.toBeInTheDocument();
    // A second project's request arrives: every card moves into a lane.
    const other = requestJson({ id: "req-o", state: "building", project: "other" });
    await listBecomes(rendered, [listed, other]);
    expect(await screen.findByRole("region", { name: "Project other", hidden: true })).toBeTruthy();
    expect(screen.getByRole("dialog", { name: "Request changes" })).toBe(dialog);
    expect(within(dialog).getByLabelText("Reason")).toHaveValue("Name the account.");
    // And goes again: back to one bare lane.
    await listBecomes(rendered, [listed]);
    await waitFor(() => {
      expect(screen.queryByRole("region", { name: "Project other", hidden: true })).toBeNull();
    });
    expect(screen.getByRole("dialog", { name: "Request changes" })).toBe(dialog);
    expect(within(dialog).getByLabelText("Reason")).toHaveValue("Name the account.");
    // Still the same stage: it can be sent.
    await userEvent.click(send(dialog));
    await waitFor(() => {
      expect(fake.sent("POST /requests/req-s/reject")).toHaveLength(1);
    });
    expect(fake.sent("POST /requests/req-s/reject")[0]?.body).toMatchObject({
      reason: "Name the account.",
    });
  });

  test("the request leaving the list closes the dialog", async () => {
    const rendered = await open();
    const { server: fake } = rendered;
    await listBecomes(rendered, [requestJson({ id: "req-o", state: "building" })]);
    await waitFor(() => {
      expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    });
    expect(fake.requests.filter((r) => r.method !== "GET")).toEqual([]);
  });
});
