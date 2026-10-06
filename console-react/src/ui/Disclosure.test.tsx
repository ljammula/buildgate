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
});
