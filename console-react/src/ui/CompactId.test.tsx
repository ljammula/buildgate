import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { CompactId } from "@/ui/CompactId";
import { middleTruncate } from "@/ui/middleTruncate";

const long = "add-subtract-numbers-to-add-py-add-a-sub-20261005-225506";

test("middleTruncate keeps the head and the tail and never exceeds max", () => {
  const short = middleTruncate(long, 28);
  expect(short).toHaveLength(28);
  expect(short.startsWith("add-subtract")).toBe(true);
  expect(short.endsWith("225506")).toBe(true);
  expect(short).toContain("…");
  expect(middleTruncate("req-halted", 28)).toBe("req-halted");
});

test("a long id shows its ends, keeps the whole value in the title and the accessibility tree", () => {
  render(<CompactId value={long} />);
  expect(screen.getByTitle(long)).toBeInTheDocument();
  expect(screen.getByText(long)).toHaveClass("sr-only");
  expect(screen.getByText(/^add-subtract.*225506$/, { selector: "[aria-hidden]" })).toBeVisible();
});

test("a short id renders as plain text", () => {
  render(<CompactId value="req-halted" />);
  expect(screen.getByText("req-halted")).toBeInTheDocument();
});

test("the copy button copies the full value, not the shortened text", async () => {
  const writeText = vi.fn(() => Promise.resolve());
  render(<CompactId value={long} label="request id" writeText={writeText} />);
  await userEvent.click(screen.getByRole("button", { name: "Copy request id" }));
  expect(writeText).toHaveBeenCalledWith(long);
  expect(await screen.findByRole("button", { name: "Copied" })).toBeInTheDocument();
});

test("copy={false} renders no button", () => {
  render(<CompactId value={long} copy={false} />);
  expect(screen.queryByRole("button")).not.toBeInTheDocument();
});
