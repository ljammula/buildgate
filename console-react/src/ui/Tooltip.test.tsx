import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { Button } from "@/ui/Button";
import { Tooltip, TooltipProvider } from "@/ui/Tooltip";

beforeAll(() => {
  globalThis.ResizeObserver ??= class {
    observe() {}
    unobserve() {}
    disconnect() {}
  };
});

test("shows its content on focus and describes the trigger", async () => {
  render(
    <TooltipProvider delayDuration={0}>
      <Tooltip content="Copy the run id">
        <Button>Copy</Button>
      </Tooltip>
    </TooltipProvider>,
  );
  expect(screen.queryByRole("tooltip")).not.toBeInTheDocument();
  await userEvent.tab();
  expect(await screen.findByRole("tooltip")).toHaveTextContent("Copy the run id");
  expect(screen.getByRole("button", { name: "Copy" })).toHaveAccessibleDescription("Copy the run id");
});
