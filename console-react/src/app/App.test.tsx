import { render, screen } from "@testing-library/react";

import { App } from "@/app/App";

test("renders the product name", () => {
  render(<App />);
  expect(screen.getByRole("heading", { name: "Buildgate" })).toBeInTheDocument();
});
