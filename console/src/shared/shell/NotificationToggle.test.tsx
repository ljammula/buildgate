import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { NotificationToggle } from "@/shared/shell/NotificationToggle";

function toggle(support: "unsupported" | "default" | "granted" | "denied", on: boolean) {
  const onTurnOn = vi.fn();
  const onTurnOff = vi.fn();
  const view = render(
    <NotificationToggle support={support} on={on} onTurnOn={onTurnOn} onTurnOff={onTurnOff} />,
  );
  return { onTurnOn, onTurnOff, ...view };
}

test("an unsupported browser gets nothing", () => {
  const { container } = toggle("unsupported", true);
  expect(container).toBeEmptyDOMElement();
});

test("with permission still to ask, the button turns notifications on", async () => {
  const { onTurnOn } = toggle("default", false);
  await userEvent.click(screen.getByRole("button", { name: "Turn on notifications" }));
  expect(onTurnOn).toHaveBeenCalledTimes(1);
});

test("granted but switched off offers the same button", async () => {
  const { onTurnOn } = toggle("granted", false);
  await userEvent.click(screen.getByRole("button", { name: "Turn on notifications" }));
  expect(onTurnOn).toHaveBeenCalledTimes(1);
});

test("granted and on says so, and a click turns it off", async () => {
  const { onTurnOff } = toggle("granted", true);
  await userEvent.click(screen.getByRole("button", { name: "Notifications on" }));
  expect(onTurnOff).toHaveBeenCalledTimes(1);
});

test("blocked in the browser is text with a hint, not a button", () => {
  toggle("denied", true);
  expect(screen.queryByRole("button")).not.toBeInTheDocument();
  const note = screen.getByText("Notifications blocked in this browser");
  expect(note).toHaveAttribute("title", expect.stringContaining("site settings"));
});
