// The spec and each ticket plan are what an approval is bound to: whole and open
// in the three review states, one closed line once the decision is behind them.
import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { oracleServer } from "@/shared/oracle/oracleTestKit";
import { json, renderApp } from "@/test/render";

import { RequestDetailScreen } from "./RequestDetailScreen";
import { openRequest, seedOperator, stubRadix } from "./testHarness";
import { requestWire, ticketWire } from "./testRequests";
import { foldsContent, planSummary, specSummary } from "./requestDetailLogic";

beforeAll(stubRadix);
beforeEach(seedOperator);

const longSpec = `# Idempotency keys\n\n## Problem\n\n${"A retried checkout charges twice. ".repeat(60)}\n\n## Acceptance criteria\n\n1. A repeated key returns the first response.\n2. A key is scoped to one account.\n\n## Risks\n\nEND-OF-SPEC\n`;
const plan = `Verify-Command: true\nAllowed-Files: a.go, b.go\n\n## Goal\n\ng\n\n### Steps\n\n1. one\n2. two\n3. three\n\nEND-OF-PLAN\n`;
const ticket = (over: Record<string, unknown> = {}) =>
  ticketWire({ index: 1, specPath: "tickets/001.spec.md", content: plan, ...over });

function foldedBlocks(): HTMLDetailsElement[] {
  return Array.from(
    document.querySelectorAll<HTMLDetailsElement>("details[data-testid^=content-]"),
  );
}

describe("review states keep what is being approved whole and open", () => {
  test("spec_review: the spec is a card, not a disclosure, with every line", async () => {
    openRequest(requestWire({ state: "spec_review", title: "T", spec: longSpec }));
    await screen.findByRole("heading", { level: 1, name: "T" });

    expect(foldedBlocks()).toHaveLength(0);
    expect(screen.getByRole("region", { name: "Spec" })).toBeInTheDocument();
    const raw = screen.getByTestId("markdown-raw-content");
    expect(raw).toBeVisible();
    expect(raw.textContent).toContain("END-OF-SPEC");
    expect(raw.textContent).toContain(longSpec.trim().split("\n")[2]);
  });

  test("plan_review: the ticket plan is a card, not a disclosure, with every line", async () => {
    openRequest(requestWire({ state: "plan_review", title: "T", tickets: [ticket()] }));
    await screen.findByRole("heading", { level: 1, name: "T" });

    expect(foldedBlocks()).toHaveLength(0);
    expect(screen.getByRole("region", { name: "Ticket 1 plan" })).toBeInTheDocument();
    const raw = screen.getByTestId("markdown-raw-content");
    expect(raw).toBeVisible();
    expect(raw.textContent).toContain("END-OF-PLAN");
  });

  test("oracle_review: the spec stays open beside the oracle files", async () => {
    const oracle = oracleServer({ "a_test.go": "package a\n" });
    oracle.state = "oracle_review";
    oracle.server.set("GET /requests/req-1", () =>
      json(requestWire({ state: "oracle_review", title: "T", spec: longSpec })),
    );
    renderApp(<RequestDetailScreen />, {
      server: oracle.server,
      path: "/requests/req-1",
      pattern: "/requests/:id",
    });
    await screen.findByRole("button", { name: "Reload files" });

    expect(foldedBlocks()).toHaveLength(0);
    expect(screen.getByRole("region", { name: "Spec" })).toBeInTheDocument();
    expect(screen.getByTestId("markdown-raw-content").textContent).toContain("END-OF-SPEC");
    expect(screen.getByTestId("oracle-review-panel")).toBeVisible();
  });
});

describe("later states fold the spec and each plan to one line, expandable in place", () => {
  test.each([
    "planning",
    "building",
    "pr_review",
    "resume_review",
    "halted",
    "quarantined",
    "done",
    "cancelled",
  ])("%s: the spec is a closed disclosure with a one-line summary", async (state) => {
    openRequest(requestWire({ state, title: "T", spec: longSpec }));
    await screen.findByRole("heading", { level: 1, name: "T" });

    const blocks = foldedBlocks();
    expect(blocks).toHaveLength(1);
    const block = blocks[0] as HTMLDetailsElement;
    expect(block.open).toBe(false);
    expect(within(block).getByRole("heading", { name: "Spec" })).toBeInTheDocument();
    expect(within(block).getByText("Idempotency keys · 2 acceptance criteria")).toBeInTheDocument();
  });

  test("expanding shows the whole text, untruncated", async () => {
    openRequest(requestWire({ state: "building", title: "T", spec: longSpec }));
    await screen.findByRole("heading", { level: 1, name: "T" });

    await userEvent.click(screen.getByRole("heading", { name: "Spec" }));

    const raw = screen.getByTestId("markdown-raw-content");
    expect(raw).toBeVisible();
    expect(raw.textContent).toContain("END-OF-SPEC");
    expect(raw.textContent).toContain(longSpec.trim().split("\n")[2]);
  });

  test("each ticket plan is its own closed disclosure naming its files and steps", async () => {
    openRequest(
      requestWire({
        state: "building",
        title: "T",
        tickets: [
          ticket({ runId: "run-1" }),
          ticketWire({ index: 2, specPath: "tickets/002.spec.md", content: plan, runId: "run-2" }),
        ],
      }),
    );
    await screen.findByRole("heading", { level: 1, name: "T" });

    const blocks = foldedBlocks();
    expect(blocks).toHaveLength(2);
    for (const block of blocks) {
      expect(block.open).toBe(false);
      expect(within(block).getByText("files: a.go, b.go · 3 steps")).toBeInTheDocument();
    }
    await userEvent.click(screen.getByRole("heading", { name: "Ticket 2 plan" }));
    expect((blocks[1] as HTMLDetailsElement).open).toBe(true);
    expect(blocks[1]?.textContent).toContain("END-OF-PLAN");
  });
});

describe("the Tickets card", () => {
  test("is absent at plan review while no ticket has a run or a pull request", async () => {
    openRequest(
      requestWire({ state: "plan_review", title: "T", tickets: [ticket(), ticket({ index: 2 })] }),
    );
    await screen.findByRole("heading", { level: 1, name: "T" });

    expect(screen.queryByTestId("tickets-section")).not.toBeInTheDocument();
    expect(screen.queryByTestId("ticket-card-1")).not.toBeInTheDocument();
  });

  test("lists only the tickets that have something to show", async () => {
    openRequest(
      requestWire({
        state: "building",
        title: "T",
        tickets: [ticket({ runId: "run-1" }), ticket({ index: 2 })],
      }),
    );
    await screen.findByRole("heading", { level: 1, name: "T" });

    expect(screen.getByTestId("ticket-card-1")).toBeInTheDocument();
    expect(screen.queryByTestId("ticket-card-2")).not.toBeInTheDocument();
  });
});

describe("foldsContent", () => {
  test("never folds a review state", () => {
    for (const state of ["spec_review", "oracle_review", "plan_review"]) {
      expect(foldsContent(state)).toBe(false);
    }
  });
});

describe("summaries", () => {
  test("a spec with no title or criteria reads spec.md", () => {
    expect(specSummary("just words")).toBe("spec.md");
  });

  test("a plan names at most three files and counts the rest", () => {
    expect(planSummary("Allowed-Files: a, b, c, d, e\n### Steps\n1. x\n")).toBe(
      "files: a, b, c +2 more · 1 step",
    );
  });

  test("a plan with neither reads plan", () => {
    expect(planSummary("## Goal\n\ng\n")).toBe("plan");
  });
});
