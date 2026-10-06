import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { Button } from "@/ui/Button";

test("is a button that does not submit a form by default", async () => {
  const onSubmit = vi.fn((event: React.SyntheticEvent) => {
    event.preventDefault();
  });
  const onClick = vi.fn();
  render(
    <form onSubmit={onSubmit}>
      <Button onClick={onClick}>Approve</Button>
    </form>,
  );
  await userEvent.click(screen.getByRole("button", { name: "Approve" }));
  expect(onClick).toHaveBeenCalledTimes(1);
  expect(onSubmit).not.toHaveBeenCalled();
});

test("a disabled button does not fire", async () => {
  const onClick = vi.fn();
  render(
    <Button disabled onClick={onClick}>
      Retry
    </Button>,
  );
  await userEvent.click(screen.getByRole("button", { name: "Retry" }));
  expect(onClick).not.toHaveBeenCalled();
});

test("asChild gives another element the button's look", () => {
  render(
    <Button asChild variant="link">
      <a href="/runs/run-1">Open run</a>
    </Button>,
  );
  const link = screen.getByRole("link", { name: "Open run" });
  expect(link).toHaveAttribute("href", "/runs/run-1");
  expect(link).not.toHaveAttribute("type");
});
