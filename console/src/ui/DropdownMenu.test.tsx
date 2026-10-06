import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { Button } from "@/ui/Button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/ui/DropdownMenu";

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

function Example({ onSelect }: { onSelect: () => void }) {
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button>Actions</Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent>
        <DropdownMenuLabel>Run</DropdownMenuLabel>
        <DropdownMenuItem onSelect={onSelect}>Retry</DropdownMenuItem>
        <DropdownMenuSeparator />
        <DropdownMenuItem disabled>Delete</DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

test("selecting an item fires onSelect and closes the menu", async () => {
  const onSelect = vi.fn();
  render(<Example onSelect={onSelect} />);
  await userEvent.click(screen.getByRole("button", { name: "Actions" }));
  await userEvent.click(await screen.findByRole("menuitem", { name: "Retry" }));
  expect(onSelect).toHaveBeenCalledTimes(1);
  expect(screen.queryByRole("menu")).not.toBeInTheDocument();
});

test("opens from the keyboard and a disabled item is flagged", async () => {
  render(<Example onSelect={vi.fn()} />);
  screen.getByRole("button", { name: "Actions" }).focus();
  await userEvent.keyboard("{Enter}");
  expect(await screen.findByRole("menu")).toBeInTheDocument();
  expect(screen.getByRole("menuitem", { name: "Delete" })).toHaveAttribute("aria-disabled", "true");
});
