import { screen, within } from "@testing-library/react";

import { openRequest, seedOperator } from "./testHarness";
import { requestWire, ticketWire } from "./testRequests";

beforeEach(seedOperator);

const SPEC =
  "# Spec\n\n## Acceptance criteria\n\n1. A repeated request returns the first response.\n" +
  "2. A key is scoped\n   to one account.\n3. A key expires after 24 hours.\n\n## Risks\n\nnone\n";

function plan(covered: string): string {
  return (
    "Verify-Command: true\nAllowed-Files: a.go\nRequired-Changed-Files: a.go\n\n## Goal\n\ng\n\n## Plan\n\n" +
    "### Files to touch\n\n- a.go\n\n### Steps\n\n1. s\n\n### Tests to add\n\n- t\n\n" +
    `### Acceptance criteria covered\n\n${covered}\n\n## Out of scope\n\nnone\n`
  );
}

const request = (state: string, plans: readonly string[], spec = SPEC) =>
  requestWire({
    state,
    title: "Idempotency keys",
    spec,
    tickets: plans.map((content, i) =>
      ticketWire({ index: i + 1, specPath: `tickets/00${i + 1}.spec.md`, content }),
    ),
  });

async function panel() {
  await screen.findByRole("heading", { level: 1, name: "Idempotency keys" });
  return screen.getByRole("region", { name: "Criteria coverage" });
}

const rowTexts = (region: HTMLElement) =>
  within(region)
    .getAllByRole("row")
    .map((row) => row.textContent);

test("plan review shows every criterion in full against the tickets that claim it", async () => {
  openRequest(request("plan_review", [plan("- 1\n- 2"), plan("- 2\n- 3")]));
  const region = await panel();

  expect(rowTexts(region)).toEqual([
    "Acceptance criterionT1Ticket 1T2Ticket 2",
    "1. A repeated request returns the first response.covers",
    "2. A key is scoped to one account.coverscovers",
    "3. A key expires after 24 hours.covers",
  ]);
  expect(within(region).getByRole("status")).toHaveTextContent(
    "Each of the 3 acceptance criteria is claimed by a ticket.",
  );
  const third = within(region).getByRole("row", { name: /A key expires/ });
  expect(
    within(third)
      .getAllByRole("cell")
      .map((cell) => cell.textContent),
  ).toEqual(["", "covers"]);
});

test("a criterion no ticket claims is called out by number and marked on its row", async () => {
  openRequest(request("plan_review", [plan("- 2")]));
  const region = await panel();

  expect(within(region).getByRole("status")).toHaveTextContent(
    "Claimed by no ticket: criteria 1, 3.",
  );
  expect(
    within(region)
      .getAllByRole("row")
      .filter((row) => row.dataset.unclaimed === "true")
      .map((row) => row.textContent),
  ).toEqual([
    "1. A repeated request returns the first response.",
    "3. A key expires after 24 hours.",
  ]);
});

test("a claim the spec has no criterion for, and a list that does not parse, are said", async () => {
  openRequest(request("plan_review", [plan("- 1\n- 2\n- 3\n- 9"), plan("- all of them")]));
  const region = await panel();

  expect(within(region).getByText("Ticket 1 claims 9: the spec has 3 criteria.")).toBeVisible();
  expect(
    within(region).getByText(
      "The covered-criteria list of ticket 2 does not parse, so it claims nothing.",
    ),
  ).toBeVisible();
});

test("the ticket files stay whole below the view: it replaces nothing the approval covers", async () => {
  openRequest(request("plan_review", [plan("- 1\n- 2\n- 3")]));
  await panel();

  const file = screen.getByRole("region", { name: "Ticket 1 plan" });
  expect(within(file).getByTestId("markdown-raw-content")).toHaveTextContent("## Out of scope");
});

test("criterion text is agent-written and is shown as text", async () => {
  const spec =
    "## Acceptance criteria\n\n1. <img src=x onerror=alert(1)> [x](javascript:alert(1))\n";
  openRequest(request("plan_review", [plan("- 1")], spec));
  const region = await panel();

  expect(region.querySelector("img, a")).toBeNull();
  expect(within(region).getByRole("rowheader")).toHaveTextContent(
    "1. <img src=x onerror=alert(1)> [x](javascript:alert(1))",
  );
});

test.each(["spec_review", "building", "done"])("absent at %s", async (state) => {
  openRequest(request(state, [plan("- 1\n- 2\n- 3")]));
  await screen.findByRole("heading", { level: 1, name: "Idempotency keys" });

  expect(screen.queryByRole("region", { name: "Criteria coverage" })).not.toBeInTheDocument();
});

test("absent when the spec has no numbered criteria", async () => {
  openRequest(request("plan_review", [plan("- 1")], "# Spec\n\nprose\n"));
  await screen.findByRole("heading", { level: 1, name: "Idempotency keys" });

  expect(screen.queryByRole("region", { name: "Criteria coverage" })).not.toBeInTheDocument();
});
