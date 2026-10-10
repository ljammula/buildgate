import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { setOperatorName } from "@/platform/operatorIdentity";
import { type FakeRoute, apiErrorResponse, json, renderApp, sseResponse } from "@/test/render";

import { requestJson, ticketJson } from "@/test/requestFixtures";
import { TriageScreen } from "./TriageScreen";

type RequestOptions = Parameters<typeof requestJson>[0];

beforeEach(() => {
  setOperatorName("operator");
});

function triage(list: RequestOptions[], details: Record<string, unknown>[] = []): FakeRoute[] {
  return [
    { on: "GET /requests", reply: () => json(list.map(requestJson)) },
    { on: "GET /requests/events", reply: sseResponse("state") },
    ...details.map((detail) => ({
      on: `GET /requests/${String(detail.id)}`,
      reply: () => json(detail),
    })),
  ];
}

const row = (id: string) => screen.queryByTestId(`triage-row-${id}`);

test("shows only needsHuman requests, oldest wait first", async () => {
  renderApp(<TriageScreen />, {
    server: triage(
      [
        { id: "req-working", state: "building", title: "Building" },
        {
          id: "req-old",
          state: "spec_review",
          title: "Old review",
          enteredAt: "2026-09-10T08:00:00Z",
        },
        {
          id: "req-new",
          state: "plan_review",
          title: "New review",
          enteredAt: "2026-09-12T08:00:00Z",
        },
      ],
      [
        {
          ...requestJson({
            id: "req-old",
            state: "spec_review",
            title: "Old review",
            enteredAt: "2026-09-10T08:00:00Z",
          }),
          spec: "# Old spec",
        },
      ],
    ),
  });
  expect(await screen.findByTestId("triage-row-req-old")).toBeInTheDocument();
  expect(row("req-working")).not.toBeInTheDocument();
  expect(row("req-new")).toBeInTheDocument();
  // Oldest wait first: the focused (first) row's detail pane shows the older request's title.
  expect(screen.getAllByText("Old review").length).toBeGreaterThan(1);
  const rows = screen.getAllByTestId(/^triage-row-/).map((el) => el.dataset.testid);
  expect(rows).toEqual(["triage-row-req-old", "triage-row-req-new"]);
  // The focused request's own artifact content, fetched separately from the board list.
  expect(await screen.findByText(/# Old spec/)).toBeInTheDocument();
});

test("lists an oracle_review request with its state, a reason and a link, and no Approve", async () => {
  renderApp(<TriageScreen />, {
    server: triage([
      {
        id: "req-oracle",
        state: "oracle_review",
        title: "Oracle",
        enteredAt: "2026-09-10T08:00:00Z",
      },
      { id: "req-spec", state: "spec_review", title: "Spec", enteredAt: "2026-09-11T08:00:00Z" },
    ]),
  });
  expect(await screen.findByTestId("triage-row-req-oracle")).toBeInTheDocument();
  expect(row("req-spec")).toBeInTheDocument();
  // The oldest wait is focused: an oracle review is opened, never approved here.
  expect(screen.getByTestId("triage-reason")).toHaveTextContent(
    "The drafted acceptance tests wait for your review.",
  );
  expect(screen.getByRole("link", { name: "Open request" })).toHaveAttribute(
    "href",
    "/requests/req-oracle",
  );
  expect(screen.queryByRole("button", { name: "Approve (a)" })).not.toBeInTheDocument();
  await userEvent.keyboard("a");
  expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  // Moving on to the spec review brings Approve and Reject back.
  await userEvent.keyboard("j");
  expect(await screen.findByRole("button", { name: "Approve (a)" })).toBeInTheDocument();
});

test("j/k move the focused row", async () => {
  renderApp(<TriageScreen />, {
    server: triage(
      [
        { id: "req-a", state: "spec_review", title: "First", enteredAt: "2026-09-10T08:00:00Z" },
        { id: "req-b", state: "spec_review", title: "Second", enteredAt: "2026-09-11T08:00:00Z" },
      ],
      [
        requestJson({ id: "req-a", state: "spec_review", title: "First" }),
        requestJson({ id: "req-b", state: "spec_review", title: "Second" }),
      ],
    ),
  });
  await screen.findByTestId("triage-row-req-a");
  const heading = () => screen.getByRole("heading", { level: 2 });
  expect(heading()).toHaveTextContent("First");

  await userEvent.keyboard("j");
  expect(heading()).toHaveTextContent("Second");
  expect(screen.getByTestId("triage-row-req-b")).toHaveAttribute("aria-current", "true");

  await userEvent.keyboard("k");
  expect(heading()).toHaveTextContent("First");

  // Clamped at both ends.
  await userEvent.keyboard("k");
  expect(heading()).toHaveTextContent("First");
});

test("a/r approve/reject the focused request through the confirm flow", async () => {
  const detail = requestJson({ id: "req-a", state: "spec_review", title: "First" });
  const { server } = renderApp(<TriageScreen />, {
    server: [
      ...triage([{ id: "req-a", state: "spec_review", title: "First" }], [detail]),
      {
        on: "POST /requests/req-a/approve",
        reply: () => json(requestJson({ id: "req-a", state: "planning", title: "First" })),
      },
      {
        on: "POST /requests/req-a/reject",
        reply: () => json(requestJson({ id: "req-a", state: "spec_drafting", title: "First" })),
      },
    ],
  });
  const approveButton = await screen.findByRole("button", { name: "Approve (a)" });
  await waitFor(() => {
    expect(approveButton).toBeEnabled();
  });

  await userEvent.keyboard("a");
  const dialog = await screen.findByRole("dialog", { name: "Approve this request?" });
  await userEvent.click(within(dialog).getByRole("button", { name: "Approve" }));

  await waitFor(() => {
    expect(server.sent("POST /requests/req-a/approve")).toHaveLength(1);
  });
  expect(server.sent("POST /requests/req-a/approve")[0]?.body).toMatchObject({ by: "operator" });
});

test("r opens the request-changes dialog for the focused request", async () => {
  const detail = requestJson({ id: "req-a", state: "spec_review", title: "First" });
  renderApp(<TriageScreen />, {
    server: triage([{ id: "req-a", state: "spec_review", title: "First" }], [detail]),
  });
  await waitFor(() => {
    expect(screen.getByRole("button", { name: "Request changes (r)" })).toBeEnabled();
  });
  await userEvent.keyboard("r");
  expect(await screen.findByRole("dialog", { name: "Request changes" })).toBeInTheDocument();
});

test("keys typed in a field are not shortcuts", async () => {
  const detail = requestJson({ id: "req-a", state: "spec_review", title: "First" });
  renderApp(<TriageScreen />, {
    server: triage([{ id: "req-a", state: "spec_review", title: "First" }], [detail]),
  });
  await waitFor(() => {
    expect(screen.getByRole("button", { name: "Request changes (r)" })).toBeEnabled();
  });
  await userEvent.keyboard("r");
  const reason = await screen.findByRole("textbox");
  await userEvent.type(reason, "a reason");
  // Neither the "a" nor the "r" typed into the reason opened another dialog.
  expect(screen.getAllByRole("dialog")).toHaveLength(1);
  expect(reason).toHaveValue("a reason");
});

test("approve stays disabled, and a is ignored, until the detail has loaded", async () => {
  const { server } = renderApp(<TriageScreen />, {
    server: triage([{ id: "req-a", state: "spec_review", title: "First" }]),
  });
  const approveButton = await screen.findByRole("button", { name: "Approve (a)" });
  await screen.findByTestId("triage-detail-error");
  expect(approveButton).toBeDisabled();
  await userEvent.keyboard("a");
  expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  expect(server.sent("POST /requests/req-a/approve")).toHaveLength(0);
});

test("shows the ticket plan, not the already-approved spec, for a plan_review request", async () => {
  renderApp(<TriageScreen />, {
    server: triage(
      [{ id: "req-a", state: "plan_review", title: "First" }],
      [
        {
          ...requestJson({
            id: "req-a",
            state: "plan_review",
            title: "First",
            tickets: [ticketJson({ index: 1, content: "Ticket plan under review" })],
          }),
          spec: "# Already-approved spec",
        },
      ],
    ),
  });
  const artifact = await screen.findByTestId("triage-artifact-content");
  expect(within(artifact).getByText(/Ticket plan under review/)).toBeInTheDocument();
  expect(within(artifact).queryByText(/Already-approved spec/)).not.toBeInTheDocument();
});

test("approve/reject actions are absent when no override token is configured", async () => {
  renderApp(<TriageScreen />, {
    server: triage([{ id: "req-a", state: "spec_review", title: "First" }]),
    config: { writesEnabled: false },
  });
  await screen.findByTestId("triage-row-req-a");
  expect(screen.queryByRole("button", { name: "Approve (a)" })).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Request changes (r)" })).not.toBeInTheDocument();

  // The 'a' shortcut must also be a no-op, not merely the button hidden.
  await userEvent.keyboard("a");
  expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
});

test("shows a message when nothing needs a human", async () => {
  renderApp(<TriageScreen />, {
    server: triage([{ id: "req-a", state: "building", title: "Building" }]),
  });
  expect(await screen.findByText("Nothing needs you right now.")).toBeInTheDocument();
});

test("lists a halted request with a reason and a link to it", async () => {
  renderApp(<TriageScreen />, {
    server: triage([{ id: "req-halted", state: "halted", title: "Halted" }]),
  });
  expect(await screen.findByTestId("triage-row-req-halted")).toBeInTheDocument();
  expect(screen.getByTestId("triage-reason")).toHaveTextContent(
    "The build halted and needs your decision.",
  );
  expect(screen.getByRole("link", { name: "Open request" })).toBeInTheDocument();
});

test("a halted request shows the server's next_action from the list entry", async () => {
  renderApp(<TriageScreen />, {
    server: [
      {
        on: "GET /requests",
        reply: () =>
          json([
            {
              ...requestJson({ id: "req-halted", state: "halted", title: "Halted" }),
              next_action: "Fix the release policy, then run `factoryd retry req-halted`.",
            },
          ]),
      },
      { on: "GET /requests/events", reply: sseResponse("state") },
    ],
  });
  expect(await screen.findByTestId("triage-row-req-halted")).toBeInTheDocument();
  expect(screen.getByTestId("triage-reason")).toHaveTextContent(
    "Fix the release policy, then run `factoryd retry req-halted`.",
  );
  expect(screen.queryByText("The build halted and needs your decision.")).not.toBeInTheDocument();
});

test("a failed load shows the error", async () => {
  renderApp(<TriageScreen />, {
    server: [{ on: "GET /requests", reply: () => apiErrorResponse(500, "boom") }],
  });
  expect(await screen.findByTestId("triage-load-error")).toHaveTextContent("Request failed (500)");
});

test("Refresh fetches the list and the focused detail again", async () => {
  const detail = requestJson({ id: "req-a", state: "spec_review", title: "First" });
  const { server } = renderApp(<TriageScreen />, {
    server: triage([{ id: "req-a", state: "spec_review", title: "First" }], [detail]),
  });
  await screen.findByRole("button", { name: "Approve (a)" });
  await waitFor(() => {
    expect(server.sent("GET /requests/req-a")).toHaveLength(1);
  });
  await userEvent.click(screen.getByRole("button", { name: "Refresh" }));
  await waitFor(() => {
    expect(server.sent("GET /requests/req-a")).toHaveLength(2);
  });
  expect(server.sent("GET /requests").length).toBeGreaterThanOrEqual(2);
});

test("the decision bar is a sticky footer that holds Approve, Reject and the key hints", async () => {
  renderApp(<TriageScreen />, {
    server: triage(
      [{ id: "req-a", state: "spec_review", title: "First" }],
      [
        {
          ...requestJson({ id: "req-a", state: "spec_review", title: "First" }),
          spec: "A long spec",
        },
      ],
    ),
  });
  const bar = await screen.findByTestId("triage-decision-bar");
  expect(bar).toHaveClass("sticky", "bottom-0");
  expect(within(bar).getByRole("button", { name: "Approve (a)" })).toBeInTheDocument();
  expect(within(bar).getByRole("button", { name: "Request changes (r)" })).toBeInTheDocument();
  const hint = within(bar).getByText(/^Keyboard:/);
  expect(hint).toHaveTextContent("Keyboard: j/k move · a approve · r request changes");
  expect([...hint.querySelectorAll("kbd")].map((k) => k.textContent)).toEqual(["j", "k", "a", "r"]);
});

test("the focused request's facts are one meta line, and the text under review stays whole", async () => {
  const spec = `# Spec\n\n${"A line of the spec under review.\n".repeat(80)}END-OF-SPEC\n`;
  renderApp(<TriageScreen />, {
    server: triage(
      [
        {
          id: "req-a",
          state: "spec_review",
          title: "First",
          project: "app",
          rejections: [
            { by: "jane", at: "2026-09-10T09:00:00Z", reason: "vague", from_state: "spec_review" },
          ],
          costSummary: {
            spec: 0,
            plan: 0,
            runs: 0,
            total: 0,
            currency: "usd",
            complete: true,
            tokens_complete: true,
            tokens: 478300,
            by_model: [{ model: "gpt-5.6-luna", tokens: 478300 }],
          },
        },
      ],
      [{ ...requestJson({ id: "req-a", state: "spec_review", title: "First" }), spec }],
    ),
  });

  const meta = await screen.findByTestId("triage-meta");
  expect(meta).toHaveTextContent("app · 478.3k tokens · 1 earlier change request");
  expect(screen.queryByText(/Usage so far/)).not.toBeInTheDocument();
  expect(screen.queryByText(/^Project:/)).not.toBeInTheDocument();
  const artifact = await screen.findByTestId("triage-artifact-content");
  expect(artifact.textContent).toContain("END-OF-SPEC");
});

test("a row is the title on one line, then the id, the project and the waiting age, the chip on the right", async () => {
  const waitingSince = new Date(Date.now() - 45 * 60_000).toISOString();
  renderApp(<TriageScreen />, {
    server: triage([
      {
        id: "req-long",
        state: "plan_review",
        title: "A very long title ".repeat(12).trim(),
        project: "checkouts",
        enteredAt: waitingSince,
        waitingSince,
      },
    ]),
  });
  const rowEl = await screen.findByTestId("triage-row-req-long");
  const title = within(rowEl).getByText(/^A very long title/);
  expect(title).toHaveClass("truncate");
  expect(title).toHaveAttribute("title", title.textContent);
  expect(within(rowEl).getByText("req-long")).toHaveClass("font-mono");
  expect(within(rowEl).getByText("checkouts")).toBeInTheDocument();
  const age = within(rowEl).getByText("for 45m");
  expect(age.tagName).toBe("TIME");
  expect(age).toHaveClass("text-fg-muted", "tabular-nums");
  expect(age.getAttribute("title")).toMatch(/^\d{4}-\d\d-\d\d \d\d:\d\d:\d\d [+-]\d\d:\d\d$/);
});
