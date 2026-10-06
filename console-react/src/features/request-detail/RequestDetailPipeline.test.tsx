import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { json } from "@/test/render";

import { openRequest, seedOperator, stubRadix } from "./testHarness";
import { historyWire, rejectionWire, requestWire, revisionWire, ticketWire } from "./testRequests";

beforeAll(stubRadix);
beforeEach(seedOperator);

const step = (name: string) => screen.getByTestId(`pipeline-step-${name}`);
const status = (name: string) => step(name).getAttribute("data-status");

describe("the pipeline stepper", () => {
  test("renders the right glyph states and entries for a 4-entry history (a reject loop revisits spec_drafting/spec_review)", async () => {
    openRequest(
      requestWire({
        state: "spec_review",
        title: "Add idempotency keys",
        history: [
          historyWire({
            from: "submitted",
            to: "spec_drafting",
            at: "2026-09-15T09:00:00Z",
            by: "factory",
          }),
          historyWire({
            from: "spec_drafting",
            to: "spec_review",
            at: "2026-09-15T09:05:00Z",
            by: "factory",
            reason: "spec drafted",
          }),
          historyWire({
            from: "spec_review",
            to: "spec_drafting",
            at: "2026-09-15T09:10:00Z",
            by: "bob",
            reason: "too vague",
          }),
          historyWire({
            from: "spec_drafting",
            to: "spec_review",
            at: "2026-09-15T09:20:00Z",
            by: "factory",
            reason: "redrafted after feedback",
          }),
        ],
      }),
    );
    await screen.findByRole("heading", { level: 1, name: "Add idempotency keys" });

    expect(status("submitted")).toBe("done");
    // spec_drafting is done too (left again after the reject loop), and the
    // entry shown is the LATEST one to reach it (bob's rejection).
    expect(status("spec_drafting")).toBe("done");
    expect(within(step("spec_drafting")).getByText(/bob/)).toBeInTheDocument();
    expect(within(step("spec_drafting")).getByText("too vague")).toBeInTheDocument();
    // spec_review is current, showing the latest (redraft) entry.
    expect(status("spec_review")).toBe("current");
    expect(within(step("spec_review")).getByText("redrafted after feedback")).toBeInTheDocument();
    expect(within(step("spec_review")).queryByText("spec drafted")).not.toBeInTheDocument();
    expect(status("planning")).toBe("pending");
  });

  test("shows a -draft-oracles request its oracle steps before it reaches them", async () => {
    openRequest(requestWire({ state: "spec_review", title: "Oracles ahead", draft_oracles: true }));
    await screen.findByRole("heading", { level: 1, name: "Oracles ahead" });
    expect(step("oracle_review")).toBeInTheDocument();
    expect(status("oracle_review")).toBe("pending");
  });

  test("marks accepted-awaiting-PR as needs-you, not failed, and survives a very long reason", async () => {
    openRequest(
      requestWire({
        state: "halted",
        halt_kind: "accepted_no_pr",
        title: "Accepted",
        history: [
          historyWire({
            from: "building",
            to: "pr_review",
            at: "2026-09-10T09:06:00Z",
            by: "factory",
          }),
          historyWire({
            from: "pr_review",
            to: "halted",
            at: "2026-09-10T09:07:00Z",
            by: "factory",
            reason: `denied by release policy ${"x".repeat(2000)}`,
          }),
        ],
      }),
    );
    await screen.findByRole("heading", { level: 1, name: "Accepted" });
    expect(status("pr_review")).toBe("needsYou");
  });

  test("hides the opt-in oracle steps entirely unless the request actually visited one", async () => {
    const pump = async (state: string, history: string[]) => {
      const view = openRequest(
        requestWire({
          state,
          title: "Oracle stage",
          history: history.slice(0, -1).map((from, i) =>
            historyWire({
              from,
              to: history[i + 1] ?? "",
              at: `2026-09-19T09:0${i}:00Z`,
              by: "factory",
            }),
          ),
        }),
      );
      await screen.findByRole("heading", { level: 1, name: "Oracle stage" });
      return view;
    };

    // Flag-less and still before planning: not rendered at all.
    let view = await pump("spec_review", ["spec_drafting", "spec_review"]);
    expect(screen.queryByTestId("pipeline-step-oracle_review")).not.toBeInTheDocument();
    view.unmount();

    // Flag-less and past the oracle stage: hidden entirely.
    view = await pump("planning", ["spec_review", "planning"]);
    for (const name of ["oracle_drafting", "oracle_review"]) {
      expect(screen.queryByTestId(`pipeline-step-${name}`)).not.toBeInTheDocument();
    }
    view.unmount();

    // oracle_review: labelled, current, the drafting step before it done.
    await pump("oracle_review", ["spec_review", "oracle_drafting", "oracle_review"]);
    expect(within(step("oracle_drafting")).getByText("Oracle drafting")).toBeInTheDocument();
    expect(status("oracle_review")).toBe("current");
    expect(status("oracle_drafting")).toBe("done");
  });

  test("lists every step in order, one row each, with 'Done' last", async () => {
    openRequest(
      requestWire({
        state: "building",
        title: "Wide layout",
        history: [
          historyWire({
            from: "submitted",
            to: "spec_drafting",
            at: "2026-09-15T09:00:00Z",
            by: "factory",
          }),
          historyWire({
            from: "plan_review",
            to: "building",
            at: "2026-09-15T09:20:00Z",
            by: "jane",
          }),
        ],
      }),
    );
    await screen.findByRole("heading", { level: 1, name: "Wide layout" });
    const items = within(screen.getByRole("list", { name: "Pipeline steps" })).getAllByRole(
      "listitem",
    );
    expect(items.map((li) => li.getAttribute("data-testid"))).toEqual(
      [
        "submitted",
        "spec_drafting",
        "spec_review",
        "planning",
        "plan_review",
        "building",
        "pr_review",
        "done",
      ].map((s) => `pipeline-step-${s}`),
    );
  });

  test("marks the failed step for a halted request, using the halt error as that step's own reason", async () => {
    openRequest(
      requestWire({
        state: "halted",
        title: "Halted mid-build",
        error: "ticket 1/2 halted: build failed",
        history: [
          historyWire({
            from: "plan_review",
            to: "building",
            at: "2026-09-15T09:00:00Z",
            by: "jane",
            reason: "approved",
          }),
          historyWire({
            from: "building",
            to: "halted",
            at: "2026-09-15T09:30:00Z",
            by: "factory",
            reason: "ticket 1/2 halted: build failed",
          }),
        ],
      }),
    );
    await screen.findByRole("heading", { level: 1, name: "Halted mid-build" });
    expect(status("building")).toBe("failed");
    expect(
      within(step("building")).getByText("ticket 1/2 halted: build failed"),
    ).toBeInTheDocument();
    // Every step after the failure point stays pending.
    expect(status("pr_review")).toBe("pending");
  });

  test("still renders a legacy request with no history, from state/enteredAt alone", async () => {
    openRequest(
      requestWire({
        state: "building",
        title: "Legacy request",
        entered_at: "2026-09-15T09:00:00Z",
        ticket_index: 1,
        ticket_count: 2,
      }),
    );
    await screen.findByRole("heading", { level: 1, name: "Legacy request" });
    expect(status("building")).toBe("current");
    // No entry to draw from, but the building step still shows ticket
    // progress and elapsed time rather than nothing.
    expect(within(step("building")).getByText(/ticket 1\/2/)).toBeInTheDocument();
  });

  test("a resume_review request marks the lost step as waiting on the operator", async () => {
    openRequest(
      requestWire({
        state: "resume_review",
        title: "Lost",
        resume: { from_state: "building", generation: 1 },
      }),
    );
    await screen.findByRole("heading", { level: 1, name: "Lost" });
    expect(status("building")).toBe("needsYou");
  });
});

describe("the order of the tickets and the plan", () => {
  const body = (state: string) =>
    requestWire({
      state,
      title: "Order",
      tickets: [
        ticketWire({
          index: 1,
          runId: "run-1",
          specPath: "tickets/001.spec.md",
          content: "Verify-Command: true\n",
        }),
      ],
    });
  const before = (a: HTMLElement, b: HTMLElement) =>
    Boolean(a.compareDocumentPosition(b) & Node.DOCUMENT_POSITION_FOLLOWING);

  test("Tickets come after the plan at plan_review", async () => {
    openRequest(body("plan_review"));
    await screen.findByRole("heading", { level: 1, name: "Order" });
    expect(
      before(
        screen.getByRole("region", { name: "Ticket 1 plan" }),
        screen.getByRole("region", { name: "Tickets" }),
      ),
    ).toBe(true);
  });

  test("Tickets lead the plan once tickets are building", async () => {
    openRequest(body("building"));
    await screen.findByRole("heading", { level: 1, name: "Order" });
    expect(
      before(
        screen.getByRole("region", { name: "Tickets" }),
        // Folded to a disclosure once building: it is a heading, not a region.
        screen.getByRole("heading", { name: "Ticket 1 plan" }),
      ),
    ).toBe(true);
  });
});

describe("compare with a rejected revision", () => {
  const stamp = () => {
    const local = new Date("2026-09-10T09:00:00Z");
    const two = (n: number) => String(n).padStart(2, "0");
    return `${local.getFullYear()}-${two(local.getMonth() + 1)}-${two(local.getDate())} ${two(
      local.getHours(),
    )}:${two(local.getMinutes())}:00`;
  };

  async function selectFirst() {
    await userEvent.click(
      await screen.findByRole("switch", { name: "Compare with rejected revision" }),
    );
    const select = await screen.findByRole("combobox", { name: "Revision" });
    await userEvent.selectOptions(select, `Revision 1 — rejected by jane at ${stamp()}`);
  }

  test("is opt-in and renders a diff once a revision is selected", async () => {
    openRequest(
      requestWire({
        state: "spec_review",
        title: "Rejected once",
        spec: "# New spec\n\nNew detail.",
        rejections: [
          rejectionWire({
            by: "jane",
            at: "2026-09-10T09:00:00Z",
            reason: "too broad",
            fromState: "spec_review",
          }),
        ],
      }),
      {
        extra: [
          {
            on: "GET /requests/req-1/revisions",
            reply: json([
              revisionWire({
                index: 1,
                at: "2026-09-10T09:00:00Z",
                by: "jane",
                reason: "too broad",
                fromState: "spec_review",
                files: ["spec.md"],
              }),
            ]),
          },
          {
            on: "GET /requests/req-1/revisions/1",
            reply: json(
              revisionWire({
                index: 1,
                at: "2026-09-10T09:00:00Z",
                by: "jane",
                reason: "too broad",
                fromState: "spec_review",
                files: { "spec.md": "# Old spec\n\nOld detail." },
              }),
            ),
          },
        ],
      },
    );
    await screen.findByRole("heading", { level: 1, name: "Rejected once" });
    // Default view: no diff, current content only.
    expect(screen.queryByTestId("revision-diff")).not.toBeInTheDocument();
    expect(screen.getByText(/New detail\./)).toBeInTheDocument();

    await selectFirst();

    const diff = await screen.findByTestId("revision-diff");
    expect(within(diff).getAllByText(/Old detail\./).length).toBeGreaterThan(0);
    expect(within(diff).getAllByText(/New detail\./).length).toBeGreaterThan(0);
  });

  test("matches a ticket by its absolute spec_path against the revision's relative key", async () => {
    openRequest(
      requestWire({
        state: "plan_review",
        title: "Rejected once",
        tickets: [
          ticketWire({
            index: 1,
            // Absolute, as the server builds it: not the bare relative key a
            // revision stores.
            specPath: "/data/requests/req-1/tickets/001.spec.md",
            content: "New ticket detail.",
          }),
        ],
        rejections: [
          rejectionWire({
            by: "jane",
            at: "2026-09-10T09:00:00Z",
            reason: "too broad",
            fromState: "plan_review",
          }),
        ],
      }),
      {
        extra: [
          {
            on: "GET /requests/req-1/revisions",
            reply: json([
              revisionWire({
                index: 1,
                at: "2026-09-10T09:00:00Z",
                by: "jane",
                reason: "too broad",
                fromState: "plan_review",
                files: ["tickets/001.spec.md"],
              }),
            ]),
          },
          {
            on: "GET /requests/req-1/revisions/1",
            reply: json(
              revisionWire({
                index: 1,
                at: "2026-09-10T09:00:00Z",
                by: "jane",
                reason: "too broad",
                fromState: "plan_review",
                files: { "tickets/001.spec.md": "Old ticket detail." },
              }),
            ),
          },
        ],
      },
    );
    await screen.findByRole("heading", { level: 1, name: "Rejected once" });
    await selectFirst();

    // Scoped to the diff itself, so this cannot pass merely because the
    // ticket's own content section shows the same text.
    const diff = await screen.findByTestId("revision-diff");
    expect(within(diff).getAllByText(/Old ticket detail\./).length).toBeGreaterThan(0);
    expect(within(diff).getAllByText(/New ticket detail\./).length).toBeGreaterThan(0);
  });

  test("a sole revision is selected for the operator", async () => {
    openRequest(
      requestWire({
        state: "spec_review",
        title: "Rejected once",
        spec: "new",
        rejections: [
          rejectionWire({
            by: "jane",
            at: "2026-09-10T09:00:00Z",
            reason: "r",
            fromState: "spec_review",
          }),
        ],
      }),
      {
        extra: [
          {
            on: "GET /requests/req-1/revisions",
            reply: json([
              revisionWire({
                index: 1,
                at: "2026-09-10T09:00:00Z",
                by: "jane",
                reason: "r",
                fromState: "spec_review",
                files: ["spec.md"],
              }),
            ]),
          },
          {
            on: "GET /requests/req-1/revisions/1",
            reply: json(
              revisionWire({
                index: 1,
                at: "2026-09-10T09:00:00Z",
                by: "jane",
                reason: "r",
                fromState: "spec_review",
                files: { "spec.md": "old" },
              }),
            ),
          },
        ],
      },
    );
    await screen.findByRole("heading", { level: 1, name: "Rejected once" });
    await userEvent.click(screen.getByRole("switch", { name: "Compare with rejected revision" }));
    expect(await screen.findByTestId("revision-diff")).toBeInTheDocument();
  });

  test("says so when no rejected revision is recorded, and shows a load failure", async () => {
    const rejected = requestWire({
      state: "spec_review",
      title: "Rejected once",
      rejections: [
        rejectionWire({
          by: "jane",
          at: "2026-09-10T09:00:00Z",
          reason: "r",
          fromState: "spec_review",
        }),
      ],
    });
    const view = openRequest(rejected, {
      extra: [{ on: "GET /requests/req-1/revisions", reply: json([]) }],
    });
    await screen.findByRole("heading", { level: 1, name: "Rejected once" });
    await userEvent.click(screen.getByRole("switch", { name: "Compare with rejected revision" }));
    expect(await screen.findByText("No rejected revisions recorded.")).toBeInTheDocument();
    view.unmount();

    openRequest(rejected, {
      extra: [
        { on: "GET /requests/req-1/revisions", reply: new Response("boom", { status: 500 }) },
      ],
    });
    await screen.findByRole("heading", { level: 1, name: "Rejected once" });
    await userEvent.click(screen.getByRole("switch", { name: "Compare with rejected revision" }));
    expect(await screen.findByText("Could not load revisions:")).toBeInTheDocument();
  });

  test("is not offered when no rejection belongs to the current stage", async () => {
    openRequest(
      requestWire({
        state: "plan_review",
        title: "Rejected at spec",
        rejections: [
          rejectionWire({
            by: "jane",
            at: "2026-09-10T09:00:00Z",
            reason: "r",
            fromState: "spec_review",
          }),
        ],
      }),
    );
    await screen.findByRole("heading", { level: 1, name: "Rejected at spec" });
    await waitFor(() => {
      expect(screen.queryByRole("region", { name: "Revisions" })).not.toBeInTheDocument();
    });
  });
});

describe("the compact stepper", () => {
  const history = [
    historyWire({
      from: "submitted",
      to: "spec_drafting",
      at: "2026-09-15T09:00:00Z",
      by: "factoryd",
    }),
    historyWire({
      from: "spec_drafting",
      to: "spec_review",
      at: "2026-09-15T09:05:00Z",
      by: "factory",
      reason: "spec drafted",
    }),
    historyWire({
      from: "spec_review",
      to: "planning",
      at: "2026-09-15T09:10:00Z",
      by: "jane",
      reason: "approved",
    }),
    historyWire({ from: "planning", to: "plan_review", at: "2026-09-15T09:15:00Z", by: "factory" }),
  ];

  test("a step the factory completed is one line, with no when, who or why", async () => {
    openRequest(requestWire({ state: "plan_review", title: "Compact", history }));
    await screen.findByRole("heading", { level: 1, name: "Compact" });

    for (const name of ["submitted", "spec_drafting", "spec_review"]) {
      expect(step(name).querySelectorAll("p")).toHaveLength(1);
    }
    expect(screen.queryByText("spec drafted")).not.toBeInTheDocument();
    expect(screen.queryByText(/factoryd?\b/)).not.toBeInTheDocument();
  });

  test("a step a person decided keeps who, when and why", async () => {
    openRequest(requestWire({ state: "plan_review", title: "Compact", history }));
    await screen.findByRole("heading", { level: 1, name: "Compact" });

    expect(within(step("planning")).getByText(/· jane$/)).toBeInTheDocument();
    expect(within(step("planning")).getByText("approved")).toBeInTheDocument();
  });

  test("the current step is emphasised and keeps its when", async () => {
    openRequest(requestWire({ state: "plan_review", title: "Compact", history }));
    await screen.findByRole("heading", { level: 1, name: "Compact" });

    expect(status("plan_review")).toBe("current");
    expect(within(step("plan_review")).getByText("Plan review")).toHaveClass("font-semibold");
    expect(step("plan_review").querySelectorAll("p").length).toBeGreaterThan(1);
  });

  test("a failed step keeps its reason, cut to a sentence with the whole text one click away", async () => {
    const reason = `ticket 1 halted: build failed. ${"detail ".repeat(60)}`;
    openRequest(
      requestWire({
        state: "halted",
        title: "Failed",
        history: [
          historyWire({
            from: "plan_review",
            to: "building",
            at: "2026-09-15T09:00:00Z",
            by: "jane",
          }),
          historyWire({
            from: "building",
            to: "halted",
            at: "2026-09-15T09:30:00Z",
            by: "factory",
            reason,
          }),
        ],
      }),
    );
    await screen.findByRole("heading", { level: 1, name: "Failed" });

    const failed = step("building");
    expect(within(failed).getByText("ticket 1 halted: build failed.")).toBeInTheDocument();
    const full = within(failed).getByRole("heading", { name: "Full reason" }).closest("details");
    expect(full?.open).toBe(false);
    expect(full?.textContent).toContain(reason.trim());
  });
});
