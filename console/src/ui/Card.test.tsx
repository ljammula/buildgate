import { render, screen } from "@testing-library/react";

import { Card, CardBody, CardFooter, CardHeader, CardTitle } from "@/ui/Card";

test("CardTitle is an h2 by default and the level is a prop", () => {
  render(
    <Card>
      <CardHeader>
        <CardTitle>Runs</CardTitle>
      </CardHeader>
      <CardBody>
        <CardTitle as="h3">Nested</CardTitle>
      </CardBody>
      <CardFooter>done</CardFooter>
    </Card>,
  );
  expect(screen.getByRole("heading", { level: 2, name: "Runs" })).toBeInTheDocument();
  expect(screen.getByRole("heading", { level: 3, name: "Nested" })).toBeInTheDocument();
});

test("passes attributes and className through", () => {
  render(
    <Card data-testid="card" className="extra" aria-label="Summary">
      body
    </Card>,
  );
  const card = screen.getByTestId("card");
  expect(card).toHaveClass("extra");
  expect(card).toHaveAttribute("aria-label", "Summary");
});
