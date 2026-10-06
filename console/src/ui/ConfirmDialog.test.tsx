import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { ConfirmDialog } from "@/ui/ConfirmDialog";

beforeAll(() => {
  Object.assign(Element.prototype, {
    hasPointerCapture: () => false,
    scrollIntoView: () => undefined,
  });
});

function setup(onConfirm: () => void | Promise<void>) {
  const onOpenChange = vi.fn();
  render(
    <ConfirmDialog
      open
      onOpenChange={onOpenChange}
      title="Cancel run?"
      confirmLabel="Cancel run"
      cancelLabel="Keep"
      tone="danger"
      onConfirm={onConfirm}
    >
      The build stops.
    </ConfirmDialog>,
  );
  return onOpenChange;
}

test("confirming runs the action once and closes on resolve", async () => {
  let resolve: () => void = () => undefined;
  const onConfirm = vi.fn(
    () =>
      new Promise<void>((r) => {
        resolve = r;
      }),
  );
  const onOpenChange = setup(onConfirm);
  expect(screen.getByRole("dialog", { name: "Cancel run?" })).toBeInTheDocument();
  const confirm = screen.getByRole("button", { name: "Cancel run" });
  await userEvent.dblClick(confirm);
  expect(onConfirm).toHaveBeenCalledTimes(1);
  expect(confirm).toBeDisabled();
  expect(onOpenChange).not.toHaveBeenCalled();
  resolve();
  await vi.waitFor(() => {
    expect(onOpenChange).toHaveBeenCalledWith(false);
  });
});

test("stays open and re-enables when the action rejects", async () => {
  const onConfirm = vi.fn(() => Promise.reject(new Error("nope")));
  const onOpenChange = setup(onConfirm);
  const confirm = screen.getByRole("button", { name: "Cancel run" });
  await userEvent.click(confirm);
  await vi.waitFor(() => expect(confirm).toBeEnabled());
  expect(onOpenChange).not.toHaveBeenCalled();
});

test("the cancel button asks to close without confirming", async () => {
  const onConfirm = vi.fn();
  const onOpenChange = setup(onConfirm);
  await userEvent.click(screen.getByRole("button", { name: "Keep" }));
  expect(onOpenChange).toHaveBeenCalledWith(false);
  expect(onConfirm).not.toHaveBeenCalled();
});
