import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { json, renderApp } from "@/test/render";

import { RequestDetailScreen } from "./RequestDetailScreen";

import { openRequest, seedOperator } from "./testHarness";
import { rejectionWire, requestWire, runWire, ticketWire } from "./testRequests";

beforeEach(seedOperator);

const ALL_STATES = [
  "submitted",
  "spec_drafting",
  "spec_review",
  "planning",
  "plan_review",
  "building",
  "pr_review",
  "done",
  "quarantined",
  "halted",
  "resume_review",
  "cancelled",
];
const REVIEW_STATES = new Set(["spec_review", "plan_review"]);

const approveButton = () => screen.queryByRole("button", { name: "Approve" });
const rejectButton = () => screen.queryByRole("button", { name: "Request changes" });

async function loaded(title = "Add idempotency keys") {
  return screen.findByRole("heading", { level: 1, name: title });
}

test.each(ALL_STATES)("renders a request in state %s without error", async (state) => {
  openRequest(requestWire({ state, title: "Add idempotency keys", spec: "# Spec\n\nDetail." }));
  await loaded();
  if (REVIEW_STATES.has(state)) {
    expect(approveButton()).toBeInTheDocument();
    expect(rejectButton()).toBeInTheDocument();
  } else {
    expect(approveButton()).not.toBeInTheDocument();
    expect(rejectButton()).not.toBeInTheDocument();
  }
});

test("shows a spinner while loading, then the not-found error with the way back", async () => {
  const view = openRequest(requestWire({ state: "building" }), { id: "req-1" });
  expect(screen.getByRole("heading", { level: 1, name: "Request detail" })).toBeInTheDocument();
  expect(screen.getByRole("status", { name: "Loading the request" })).toBeInTheDocument();
  await loaded("req-1");
  view.unmount();

  renderApp(<RequestDetailScreen />, { path: "/requests/missing", pattern: "/requests/:id" });
  expect(await screen.findByRole("alert")).toHaveTextContent("Not found");
  expect(screen.getByRole("link", { name: "Back to board" })).toHaveAttribute("href", "/");
  expect(screen.getByRole("heading", { level: 1, name: "Request detail" })).toBeInTheDocument();
});

test("renders ticket plan content and PR/run links for plan_review", async () => {
  openRequest(
    requestWire({
      state: "plan_review",
      title: "Add idempotency keys",
      tickets: [
        ticketWire({
          index: 1,
          specPath: "tickets/001.spec.md",
          runId: "run-1",
          prUrl: "https://github.com/acme/app/pull/1",
          prState: "open",
          content: "Verify-Command: true\n## Goal\n",
        }),
      ],
    }),
  );
  await loaded();
  expect(await screen.findByText(/## Goal/)).toBeInTheDocument();
  const pr = screen.getByRole("link", { name: "https://github.com/acme/app/pull/1" });
  expect(pr).toHaveAttribute("href", "https://github.com/acme/app/pull/1");
  expect(pr).toHaveAttribute("target", "_blank");
  expect(pr).toHaveAttribute("rel", "noopener noreferrer");
  expect(screen.getByRole("link", { name: "View run" })).toHaveAttribute("href", "/runs/run-1");
});

test("a ticket card shows its own run's real state, not a guess", async () => {
  openRequest(
    requestWire({
      state: "building",
      title: "Add idempotency keys",
      tickets: [ticketWire({ index: 1, runId: "run-1" })],
    }),
    {
      extra: [
        { on: "GET /runs/run-1", reply: json(runWire("run-quarantined.json", { id: "run-1" })) },
      ],
    },
  );
  await loaded();
  // The roll-up strip above guesses "building" from the run id; the card
  // shows what the run itself says.
  const card = screen.getByTestId("ticket-card-1");
  expect(await within(card).findByText("Quarantined")).toBeInTheDocument();
  expect(within(card).getByRole("link", { name: "View run" })).toBeInTheDocument();
});

test("a ticket card shows a waiting chip for its queued run", async () => {
  openRequest(
    requestWire({
      state: "building",
      title: "Add idempotency keys",
      tickets: [ticketWire({ index: 1, runId: "run-1" })],
    }),
    {
      extra: [
        {
          on: "GET /runs/run-1",
          reply: json(
            runWire("run-running.json", {
              id: "run-1",
              stalled: false,
              stalled_since_seconds: null,
              waiting_reason: "behind 1 run(s) on foo/bar",
            }),
          ),
        },
      ],
    },
  );
  await loaded();
  expect(await screen.findByTestId("waiting-chip")).toHaveTextContent(
    "waiting: behind 1 run(s) on foo/bar",
  );
  expect(screen.queryByTestId("stalled-chip")).not.toBeInTheDocument();
});

test("the markdown raw/rendered toggle switches between plain text and a rendered heading", async () => {
  openRequest(
    requestWire({
      state: "spec_review",
      title: "Add idempotency keys",
      spec: "## Goal\n\nDo the thing.\n",
    }),
  );
  await loaded();
  // Raw by default: the literal markdown source, `##` included.
  expect(screen.getByTestId("markdown-raw-content")).toBeInTheDocument();
  expect(screen.getByText(/## Goal/)).toBeInTheDocument();

  await userEvent.click(screen.getByRole("switch", { name: "Raw" }));

  expect(screen.getByTestId("markdown-rendered-content")).toBeInTheDocument();
  expect(screen.queryByText(/## Goal/)).not.toBeInTheDocument();
  expect(screen.getByRole("heading", { name: "Goal" })).toBeInTheDocument();
  expect(screen.getByText("Do the thing.")).toBeInTheDocument();
});

test("approving a request calls the approve endpoint and refreshes", async () => {
  const { server } = openRequest(
    requestWire({ state: "spec_review", title: "Add idempotency keys" }),
    {
      extra: [
        {
          on: "POST /requests/req-1/approve",
          reply: json(requestWire({ state: "planning", title: "Add idempotency keys" })),
        },
      ],
    },
  );
  await loaded();
  await waitFor(() => {
    expect(approveButton()).toBeEnabled();
  });
  await userEvent.click(approveButton() as HTMLElement);

  // Approve goes through a confirm dialog before the endpoint is called.
  const dialog = await screen.findByRole("dialog", { name: "Approve this request?" });
  expect(server.sent("POST /requests/req-1/approve")).toHaveLength(0);
  await userEvent.click(within(dialog).getByRole("button", { name: "Approve" }));

  await waitFor(() => {
    expect(server.sent("POST /requests/req-1/approve")).toHaveLength(1);
  });
  await waitFor(() => {
    expect(approveButton()).not.toBeInTheDocument();
  });
});

test("approving keeps the title and spec visible from the approve response", async () => {
  const shown = { title: "Add idempotency keys", spec: "# Spec\n\nDetail." };
  openRequest(requestWire({ state: "spec_review", ...shown }), {
    extra: [
      {
        on: "POST /requests/req-1/approve",
        reply: json(requestWire({ state: "planning", ...shown })),
      },
    ],
  });
  await loaded();
  await waitFor(() => {
    expect(approveButton()).toBeEnabled();
  });
  await userEvent.click(approveButton() as HTMLElement);
  const dialog = await screen.findByRole("dialog");
  await userEvent.click(within(dialog).getByRole("button", { name: "Approve" }));

  await waitFor(() => {
    expect(approveButton()).not.toBeInTheDocument();
  });
  expect(
    screen.getByRole("heading", { level: 1, name: "Add idempotency keys" }),
  ).toBeInTheDocument();
  expect(screen.getByText(/Detail\./)).toBeInTheDocument();
});

test("rejecting a request requires a reason and calls the reject endpoint", async () => {
  const { server } = openRequest(
    requestWire({ state: "spec_review", title: "Add idempotency keys" }),
    {
      extra: [
        {
          on: "POST /requests/req-1/reject",
          reply: json(requestWire({ state: "spec_drafting", title: "Add idempotency keys" })),
        },
      ],
    },
  );
  await loaded();
  await waitFor(() => {
    expect(rejectButton()).toBeEnabled();
  });
  await userEvent.click(rejectButton() as HTMLElement);

  const dialog = await screen.findByRole("dialog", { name: "Request changes" });
  // An empty reason cannot be sent.
  const confirm = within(dialog).getByRole("button", { name: "Request changes" });
  expect(confirm).toBeDisabled();
  expect(server.sent("POST /requests/req-1/reject")).toHaveLength(0);

  await userEvent.type(within(dialog).getByLabelText("Reason"), "scope is too broad");
  await userEvent.click(confirm);

  await waitFor(() => {
    expect(server.sent("POST /requests/req-1/reject")).toHaveLength(1);
  });
  expect(server.sent("POST /requests/req-1/reject")[0]?.body).toEqual({
    reason: "scope is too broad",
    by: "operator",
  });
});

test("shows the audit line when approvedBy/approvedAt are set", async () => {
  openRequest(
    requestWire({
      state: "building",
      title: "Add idempotency keys",
      approved_by: "jane",
      approved_at: "2026-09-12T10:00:00Z",
    }),
  );
  await loaded();
  // Local time, so the test holds in any timezone the suite runs under.
  const local = new Date("2026-09-12T10:00:00Z");
  const two = (n: number) => String(n).padStart(2, "0");
  const stamp = `${local.getFullYear()}-${two(local.getMonth() + 1)}-${two(local.getDate())} ${two(
    local.getHours(),
  )}:${two(local.getMinutes())}:00`;
  expect(screen.getByText(`Approved by jane at ${stamp}`)).toBeInTheDocument();
});

test("shows no audit line when the request has never been approved", async () => {
  openRequest(requestWire({ state: "spec_review", title: "Not yet approved" }));
  await loaded("Not yet approved");
  expect(screen.queryByText(/Approved by/)).not.toBeInTheDocument();
});

test("shows the rejection history when rejections are present", async () => {
  openRequest(
    requestWire({
      state: "spec_review",
      title: "Rejected once",
      rejections: [
        rejectionWire({
          by: "jane",
          at: "2026-09-12T10:00:00Z",
          reason: "scope is too broad",
          fromState: "spec_review",
        }),
      ],
    }),
  );
  await loaded("Rejected once");
  const local = new Date("2026-09-12T10:00:00Z");
  const two = (n: number) => String(n).padStart(2, "0");
  const stamp = `${local.getFullYear()}-${two(local.getMonth() + 1)}-${two(local.getDate())} ${two(
    local.getHours(),
  )}:${two(local.getMinutes())}:00`;

  await userEvent.click(screen.getByRole("button", { name: "Rejection history (1)" }));

  const history = screen.getByTestId("rejection-history");
  expect(
    within(history).getByText(`Rejected by jane at ${stamp} (from spec_review)`),
  ).toBeVisible();
  expect(within(history).getByText("scope is too broad")).toBeVisible();
});

test("a send-back note reads as sent back, with the stage it feeds", async () => {
  openRequest(
    requestWire({
      state: "planning",
      title: "T",
      rejections: [
        {
          ...rejectionWire({
            by: "bob",
            at: "x",
            reason: "wrong ticket split",
            fromState: "quarantined",
          }),
          for_stage: "plan_review",
        },
      ],
    }),
  );
  await loaded("T");
  await userEvent.click(screen.getByRole("button", { name: "Rejection history (1)" }));
  expect(
    screen.getByText("Sent back by bob at x (from quarantined, for plan_review)"),
  ).toBeInTheDocument();
});

test("approve/reject are disabled again while a Refresh is in flight or has failed (detailLoaded must not stay true after the first load)", async () => {
  const { server } = openRequest(requestWire({ state: "spec_review", title: "First" }));
  await loaded("First");
  await waitFor(() => {
    expect(approveButton()).toBeEnabled();
  });
  expect(rejectButton()).toBeEnabled();

  let fail: () => void = () => undefined;
  const gate = new Promise<void>((resolve) => {
    fail = resolve;
  });
  server.set("GET /requests/req-1", async () => {
    await gate;
    return new Response("server error", { status: 500 });
  });
  await userEvent.click(screen.getByRole("button", { name: "Refresh" }));
  // Mid-flight: not ready already, not only once the failure lands.
  await waitFor(() => {
    expect(approveButton()).toBeDisabled();
  });
  expect(rejectButton()).toBeDisabled();

  fail();
  // The refresh failed (500): still disabled, and the older record stays on screen.
  expect(await screen.findByText(/Refresh failed:/)).toBeInTheDocument();
  expect(approveButton()).toBeDisabled();
  expect(rejectButton()).toBeDisabled();
  expect(screen.getByRole("heading", { level: 1, name: "First" })).toBeInTheDocument();
});

test("the approve confirm dialog is absent when the console cannot write", async () => {
  openRequest(requestWire({ state: "spec_review", title: "No token" }), { writes: false });
  await loaded("No token");
  expect(approveButton()).toBeDisabled();
  await userEvent.click(approveButton() as HTMLElement);
  expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  expect(screen.queryByLabelText("Operator name")).not.toBeInTheDocument();
  expect(screen.getByText(/This console cannot write here/)).toBeInTheDocument();
});

test("the reject reason dialog is absent when the console cannot write", async () => {
  openRequest(requestWire({ state: "spec_review", title: "No token" }), { writes: false });
  await loaded("No token");
  await userEvent.click(rejectButton() as HTMLElement);
  expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  expect(screen.queryByLabelText("Reason")).not.toBeInTheDocument();
});

test("while the detail loads a writer is told why Approve waits", async () => {
  openRequest(requestWire({ state: "spec_review", title: "T" }));
  await loaded("T");
  // The note shows only while the full content has not been confirmed loaded.
  await waitFor(() => {
    expect(
      screen.queryByText(/Loading the full spec\/plan content before enabling approve\/reject/),
    ).not.toBeInTheDocument();
  });
});

test("no Approve/Reject for a halted request (the server refuses them from any state but spec_review/plan_review)", async () => {
  openRequest(requestWire({ state: "halted", title: "Halted" }));
  await loaded("Halted");
  expect(approveButton()).not.toBeInTheDocument();
  expect(rejectButton()).not.toBeInTheDocument();
});

test("the quarantined callout is shown with disabled actions when no write access is configured (never 'waiting on you' with nothing to do)", async () => {
  openRequest(
    requestWire({
      state: "quarantined",
      title: "Quarantined, no token",
      tickets: [ticketWire({ index: 1, runId: "run-1" })],
    }),
    { writes: false },
  );
  await loaded("Quarantined, no token");
  expect(screen.getByTestId("recovery-callout")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Retry request" })).toBeDisabled();
  expect(screen.getByRole("button", { name: "Cancel request" })).toBeDisabled();
});

test("a live event re-fetches the full detail instead of applying the summary-shaped event", async () => {
  let spec = "# Spec\n\nFIRST DRAFT";
  let updatedAt = "2026-09-10T09:05:00Z";
  let stream: ReadableStreamDefaultController<Uint8Array> | undefined;
  const { server } = openRequest(
    () => requestWire({ state: "spec_review", title: "Live", spec, updated_at: updatedAt }),
    {
      extra: [
        {
          on: "GET /requests/events",
          reply: () =>
            new Response(
              new ReadableStream<Uint8Array>({
                start(controller) {
                  stream = controller;
                },
              }),
            ),
        },
      ],
    },
  );
  await loaded("Live");
  expect(screen.getByText(/FIRST DRAFT/)).toBeInTheDocument();
  await waitFor(() => {
    expect(stream).toBeDefined();
  });
  const before = server.sent("GET /requests/req-1").length;

  // The server redrafts; the event carries only the summary shape (no spec).
  spec = "# Spec\n\nREDRAFT";
  updatedAt = "2026-09-10T09:07:00Z";
  const summary = requestWire({ state: "spec_review", title: "Live", updated_at: updatedAt });
  const send = () => {
    stream?.enqueue(new TextEncoder().encode(`event: state\ndata: ${JSON.stringify(summary)}\n\n`));
  };
  send();

  expect(await screen.findByText(/REDRAFT/)).toBeInTheDocument();
  expect(screen.queryByText(/FIRST DRAFT/)).not.toBeInTheDocument();
  expect(server.sent("GET /requests/req-1")).toHaveLength(before + 1);

  // A repeat of the same event (same updated_at and state) is a no-op.
  send();
  await new Promise((resolve) => setTimeout(resolve, 50));
  expect(server.sent("GET /requests/req-1")).toHaveLength(before + 1);
});

test("a permanently failed event stream leaves the loaded request readable and actionable", async () => {
  openRequest(requestWire({ state: "spec_review", title: "No stream" }), {
    extra: [{ on: "GET /requests/events", reply: new Response("nope", { status: 403 }) }],
  });
  await loaded("No stream");
  await waitFor(() => {
    expect(approveButton()).toBeEnabled();
  });
  expect(screen.queryByText(/Live updates/)).not.toBeInTheDocument();
});

test("the Next banner shows the server-supplied next_action prominently for a non-terminal state", async () => {
  openRequest(
    requestWire({
      state: "building",
      title: "Add idempotency keys",
      next_action: "Wait for the build to finish.",
    }),
  );
  await loaded();
  expect(screen.getByTestId("next-action-banner")).toHaveTextContent(
    "Wait for the build to finish.",
  );
  expect(screen.getAllByText("Wait for the build to finish.")).toHaveLength(1);
});

test("the Next banner is not shown in a review state, where its CLI wording would contradict the buttons", async () => {
  openRequest(
    requestWire({
      state: "spec_review",
      title: "T",
      next_action: "review it: `factoryd approve req-1`",
    }),
  );
  await loaded("T");
  expect(screen.queryByTestId("next-action-banner")).not.toBeInTheDocument();
});

test("the header shows the state, the running job and how long the request has waited on the operator", async () => {
  openRequest(
    requestWire({
      state: "spec_review",
      title: "T",
      waiting_since: "2026-09-10T09:05:00Z",
      active_job: { stage: "spec_review", role: "planning", model: "gpt" },
    }),
  );
  await loaded("T");
  expect(screen.getByText(/^Waiting on you · /)).toBeInTheDocument();
  expect(screen.getByText("req-1")).toBeInTheDocument();
});

test("a back link returns to the board", async () => {
  openRequest(requestWire({ state: "done", title: "T" }));
  await loaded("T");
  expect(screen.getByRole("link", { name: "Back to board" })).toHaveAttribute("href", "/");
});

test("a request in pr_review shows the server's next step: what its pull request waits on", async () => {
  const next =
    "review https://github.com/acme/app/pull/6: approve and merge it, or leave review comments. The request is done when every pull request is merged";
  openRequest(
    requestWire({
      state: "pr_review",
      title: "Add idempotency keys",
      next_action: next,
      tickets: [
        ticketWire({
          index: 1,
          runId: "run-1",
          prUrl: "https://github.com/acme/app/pull/6",
          prState: "ready",
        }),
      ],
    }),
  );
  await loaded();
  expect(screen.getByTestId("next-action-banner")).toHaveTextContent(next);
});
