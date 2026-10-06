import { act, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { CopyableCommand } from "@/ui/CopyableCommand";

test("copies the exact command and the button name becomes Copied", async () => {
  const writeText = vi.fn(() => Promise.resolve());
  render(<CopyableCommand command="factoryd approve run-1" label="Approve" writeText={writeText} />);
  expect(screen.getByText("factoryd approve run-1")).toBeInTheDocument();
  await userEvent.click(screen.getByRole("button", { name: "Copy command" }));
  expect(writeText).toHaveBeenCalledWith("factoryd approve run-1");
  expect(await screen.findByRole("button", { name: "Copied" })).toBeInTheDocument();
});

test("the name reverts after 2 seconds", async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  try {
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime });
    render(<CopyableCommand command="ls" writeText={() => Promise.resolve()} />);
    await user.click(screen.getByRole("button", { name: "Copy command" }));
    expect(await screen.findByRole("button", { name: "Copied" })).toBeInTheDocument();
    act(() => {
      vi.advanceTimersByTime(2100);
    });
    expect(screen.getByRole("button", { name: "Copy command" })).toBeInTheDocument();
  } finally {
    vi.useRealTimers();
  }
});

test("a failed copy does not claim success", async () => {
  render(<CopyableCommand command="ls" writeText={() => Promise.reject(new Error("denied"))} />);
  await userEvent.click(screen.getByRole("button", { name: "Copy command" }));
  expect(screen.getByRole("button", { name: "Copy command" })).toBeInTheDocument();
});
