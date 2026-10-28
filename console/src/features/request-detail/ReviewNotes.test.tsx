import { screen, within } from "@testing-library/react";

import { openRequest } from "./testHarness";
import { historyWire, requestWire } from "./testRequests";

const notes = [
  "criterion 2 names `handler.go`, which ticket 001.spec.md may change, but the tickets covering criterion 2 may not",
  "criterion 3 says `README.md` changes, but no ticket may change it",
];

test("a plan under review shows the factory's notes, each file name as code", async () => {
  openRequest(requestWire({ state: "plan_review", title: "T", review_notes: notes }));
  const panel = within(await screen.findByTestId("review-notes"));
  expect(panel.getByText("Notes from the factory")).toBeInTheDocument();
  expect(panel.getAllByRole("listitem")).toHaveLength(2);
  expect(panel.getByText("handler.go").tagName).toBe("CODE");
  expect(panel.getByText(/Advice only/)).toBeInTheDocument();
});

test("a spec under review shows them too", async () => {
  openRequest(
    requestWire({
      state: "spec_review",
      title: "T",
      review_notes: ["criterion 1 reads as a rule for the commit message"],
    }),
  );
  expect(await screen.findByTestId("review-notes")).toHaveTextContent(
    "criterion 1 reads as a rule",
  );
});

test("a build on a plan the factory approved itself shows the notes, labelled as unreviewed", async () => {
  openRequest(
    requestWire({
      state: "building",
      title: "T",
      review_notes: notes,
      history: [
        historyWire({
          from: "planning",
          to: "building",
          at: "2026-09-15T09:00:00Z",
          by: "factoryd",
        }),
      ],
    }),
  );
  const panel = within(await screen.findByTestId("review-notes"));
  expect(panel.getByText("Plan notes (plan review skipped)")).toBeInTheDocument();
  expect(panel.getAllByRole("listitem")).toHaveLength(2);
  expect(panel.getByText(/no person reviewed it/)).toBeInTheDocument();
});

test("a build after a person's plan review shows no notes", async () => {
  openRequest(
    requestWire({
      state: "building",
      title: "T",
      review_notes: notes,
      history: [
        historyWire({
          from: "planning",
          to: "plan_review",
          at: "2026-09-15T09:00:00Z",
          by: "factoryd",
        }),
        historyWire({
          from: "plan_review",
          to: "building",
          at: "2026-09-15T09:05:00Z",
          by: "jane",
        }),
      ],
    }),
  );
  await screen.findByRole("heading", { level: 1, name: "T" });
  expect(screen.queryByTestId("review-notes")).toBeNull();
});

test("no notes, or a state that is not a review of a spec or plan: nothing is drawn", async () => {
  openRequest(requestWire({ state: "plan_review", title: "T" }));
  await screen.findByRole("heading", { level: 1, name: "T" });
  expect(screen.queryByTestId("review-notes")).toBeNull();
});
