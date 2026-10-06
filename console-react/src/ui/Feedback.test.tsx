import { render, screen } from "@testing-library/react";

import { Callout, EmptyState, Skeleton, Spinner } from "@/ui/Feedback";

test("Callout is an alert for danger and a status otherwise", () => {
  render(
    <>
      <Callout tone="danger" title="Build failed">
        Tests red
      </Callout>
      <Callout tone="info">FYI</Callout>
    </>,
  );
  expect(screen.getByRole("alert")).toHaveTextContent("Build failed");
  expect(screen.getByRole("status")).toHaveTextContent("FYI");
});

test("Spinner has an accessible name and Skeleton is hidden from assistive tech", () => {
  render(
    <>
      <Spinner label="Loading runs" />
      <Skeleton data-testid="sk" />
    </>,
  );
  expect(screen.getByRole("status", { name: "Loading runs" })).toBeInTheDocument();
  expect(screen.getByTestId("sk")).toHaveAttribute("aria-hidden", "true");
});

test("EmptyState shows title, body and action", () => {
  render(
    <EmptyState title="No runs" action={<button type="button">New run</button>}>
      Submit a ticket.
    </EmptyState>,
  );
  expect(screen.getByText("No runs")).toBeInTheDocument();
  expect(screen.getByText("Submit a ticket.")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "New run" })).toBeInTheDocument();
});
