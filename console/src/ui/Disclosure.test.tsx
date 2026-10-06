import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { Disclosure } from "@/ui/Disclosure";

function details(): HTMLDetailsElement {
  const el = document.querySelector("details");
  if (el === null) throw new Error("no details element");
  return el;
}

describe("Disclosure", () => {
  test("is closed by default, names itself with a heading and says what is inside", () => {
    render(
      <Disclosure title="Attempts" summary="1 attempt · exit 0">
        <p>body</p>
      </Disclosure>,
    );

    expect(screen.getByRole("heading", { name: "Attempts" })).toBeInTheDocument();
    expect(screen.getByText("1 attempt · exit 0")).toBeInTheDocument();
    expect(details().open).toBe(false);
  });

  test("opens in place when the summary is clicked", async () => {
    render(
      <Disclosure title="Attempts">
        <p>body</p>
      </Disclosure>,
    );

    await userEvent.click(screen.getByText("Attempts"));

    expect(details().open).toBe(true);
    expect(screen.getByText("body")).toBeVisible();
  });

  test("defaultOpen starts it open, and the operator's own close sticks across a re-render", async () => {
    const { rerender } = render(
      <Disclosure title="Gate results" defaultOpen>
        <p>body</p>
      </Disclosure>,
    );
    expect(details().open).toBe(true);

    await userEvent.click(screen.getByText("Gate results"));
    expect(details().open).toBe(false);

    rerender(
      <Disclosure title="Gate results" defaultOpen>
        <p>body</p>
      </Disclosure>,
    );
    expect(details().open).toBe(false);
  });

  test("the summary reports aria-expanded as it toggles", async () => {
    render(
      <Disclosure title="Attempts">
        <p>body</p>
      </Disclosure>,
    );
    const summary = screen.getByText("Attempts").closest("summary");
    expect(summary).toHaveAttribute("aria-expanded", "false");
    await userEvent.click(screen.getByText("Attempts"));
    expect(summary).toHaveAttribute("aria-expanded", "true");
  });

  test("onOpenChange hears each toggle, controlled or not", async () => {
    const onOpenChange = vi.fn();
    render(
      <Disclosure title="Attempts" onOpenChange={onOpenChange}>
        <p>body</p>
      </Disclosure>,
    );
    await userEvent.click(screen.getByText("Attempts"));
    await userEvent.click(screen.getByText("Attempts"));
    expect(onOpenChange.mock.calls).toEqual([[true], [false]]);
  });

  test("controlled: open decides, and a click only asks", async () => {
    const onOpenChange = vi.fn();
    const { rerender } = render(
      <Disclosure title="Attempts" open={false} onOpenChange={onOpenChange}>
        <p>body</p>
      </Disclosure>,
    );
    await userEvent.click(screen.getByText("Attempts"));
    expect(onOpenChange).toHaveBeenCalledWith(true);
    expect(details().open).toBe(false);

    rerender(
      <Disclosure title="Attempts" open onOpenChange={onOpenChange}>
        <p>body</p>
      </Disclosure>,
    );
    expect(details().open).toBe(true);
    await userEvent.click(screen.getByText("Attempts"));
    expect(onOpenChange).toHaveBeenLastCalledWith(false);
    expect(details().open).toBe(true);
  });

  test("headingLevel null: the title is plain text and the summary is a named button", async () => {
    render(
      <Disclosure title="Rejection history (2)" headingLevel={null}>
        <p>body</p>
      </Disclosure>,
    );
    expect(screen.queryByRole("heading")).not.toBeInTheDocument();
    const toggle = screen.getByRole("button", { name: "Rejection history (2)" });
    expect(toggle).toHaveAttribute("aria-expanded", "false");
    await userEvent.click(toggle);
    expect(toggle).toHaveAttribute("aria-expanded", "true");
  });

  test("a failed disclosure is marked", () => {
    render(
      <Disclosure title="Gate" failed defaultOpen>
        <p>body</p>
      </Disclosure>,
    );
    expect(details()).toHaveAttribute("data-failed", "true");
  });
});
