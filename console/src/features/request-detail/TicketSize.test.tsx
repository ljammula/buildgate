// What each ticket's plan says about its size: the planner's estimate against
// the budget the plan was drafted under, and the built run's actual lines.
import { screen } from "@testing-library/react";

import { decodeRequestTicket } from "@/domain/request";

import { sizeLine } from "./requestDetailLogic";
import { openRequest, seedOperator } from "./testHarness";
import { requestWire, ticketWire } from "./testRequests";

beforeEach(seedOperator);

const plan = "Verify-Command: true\nAllowed-Files: a.go\n\n## Goal\n\ng\n";
const ticket = (estimatedChangedLines: number | undefined, actualChangedLines?: number) =>
  decodeRequestTicket(
    ticketWire({
      index: 1,
      specPath: "tickets/001.spec.md",
      content: plan,
      ...(estimatedChangedLines === undefined ? {} : { estimatedChangedLines }),
      ...(actualChangedLines === undefined ? {} : { actualChangedLines }),
    }),
    "ticket",
  );

describe("sizeLine", () => {
  test("an estimate within the budget, over it, and with no budget", () => {
    expect(sizeLine(ticket(220), 400)).toBe(
      "Estimated 220 changed lines, within the 400-line budget",
    );
    expect(sizeLine(ticket(620), 400)).toBe(
      "Estimated 620 changed lines, over the 400-line budget",
    );
    expect(sizeLine(ticket(620), 0)).toBe("Estimated 620 changed lines");
  });

  test("the built run's actual lines follow the estimate", () => {
    expect(sizeLine(ticket(220, 251), 400)).toBe(
      "Estimated 220 changed lines, within the 400-line budget · built: 251 changed lines",
    );
    expect(sizeLine(ticket(undefined, 51), 400)).toBe("built: 51 changed lines");
  });

  test("a ticket with neither says nothing", () => {
    expect(sizeLine(ticket(undefined), 400)).toBe("");
  });
});

test("plan review shows each ticket's estimate against the recorded budget", async () => {
  openRequest(
    requestWire({
      state: "plan_review",
      title: "T",
      plan_budget_lines: 400,
      tickets: [
        ticketWire({
          index: 1,
          specPath: "tickets/001.spec.md",
          content: plan,
          estimatedChangedLines: 220,
        }),
        ticketWire({
          index: 2,
          specPath: "tickets/002.spec.md",
          content: plan,
          estimatedChangedLines: 620,
        }),
        ticketWire({ index: 3, specPath: "tickets/003.spec.md", content: plan }),
      ],
    }),
  );
  expect(await screen.findByTestId("ticket-size-1")).toHaveTextContent(
    "Ticket 1: Estimated 220 changed lines, within the 400-line budget",
  );
  expect(screen.getByTestId("ticket-size-2")).toHaveTextContent("over the 400-line budget");
  expect(screen.queryByTestId("ticket-size-3")).toBeNull();
});
