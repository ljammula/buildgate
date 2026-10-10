import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Link } from "react-router";

import { IconButton } from "@/ui/IconButton";
import { TooltipProvider } from "@/ui/Tooltip";

beforeAll(() => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      unobserve() {}
      disconnect() {}
    },
  );
});

test("is named by its label and shows the same text as a tooltip", async () => {
  render(
    <TooltipProvider delayDuration={0}>
      <IconButton label="Refresh">
        <svg aria-hidden="true" />
      </IconButton>
    </TooltipProvider>,
  );
  expect(screen.getByRole("button", { name: "Refresh" })).toHaveAttribute("aria-label", "Refresh");
  await userEvent.tab();
  expect(await screen.findByRole("tooltip")).toHaveTextContent("Refresh");
});

test("with asChild the child stays a link", () => {
  render(
    <MemoryRouter>
      <TooltipProvider>
        <IconButton asChild label="Docs">
          <Link to="/docs">
            <svg aria-hidden="true" />
          </Link>
        </IconButton>
      </TooltipProvider>
    </MemoryRouter>,
  );
  expect(screen.getByRole("link", { name: "Docs" })).toHaveAttribute("href", "/docs");
});
