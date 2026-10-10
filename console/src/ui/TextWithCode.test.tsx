import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { TextWithCode, splitBackticks } from "@/ui/TextWithCode";

test("text between backticks becomes code, the rest stays text", () => {
  const { container } = render(
    <p>
      <TextWithCode text="Run `factoryd worker` to start it, or `factoryd doctor`." />
    </p>,
  );
  const codes = [...container.querySelectorAll("code")].map((c) => c.textContent);
  expect(codes).toEqual(["factoryd worker", "factoryd doctor"]);
  expect(container.textContent).toBe("Run factoryd worker to start it, or factoryd doctor.");
});

test("an unbalanced backtick renders as written", () => {
  const { container } = render(
    <p>
      <TextWithCode text="a `b` c ` d" />
    </p>,
  );
  expect([...container.querySelectorAll("code")].map((c) => c.textContent)).toEqual(["b"]);
  expect(container.textContent).toBe("a b c ` d");
  expect(splitBackticks("no `closing")).toEqual([{ code: false, value: "no `closing" }]);
});

test("markup in the text is text, never HTML", () => {
  render(
    <p>
      <TextWithCode text="`<b>bold</b>` and <i>plain</i>" />
    </p>,
  );
  expect(screen.getByText("<b>bold</b>")).toBeInTheDocument();
  expect(document.querySelector("b")).toBeNull();
  expect(document.querySelector("i")).toBeNull();
});

test("an empty pair stays as written", () => {
  expect(splitBackticks("a `` b")).toEqual([
    { code: false, value: "a " },
    { code: false, value: "``" },
    { code: false, value: " b" },
  ]);
});

describe("copy buttons", () => {
  test("each code span has a button that writes exactly its text", async () => {
    const writeText = vi.fn(() => Promise.resolve());
    render(
      <p>
        <TextWithCode
          text="Run `factoryd worker` or `factoryd doctor -fix`."
          writeText={writeText}
        />
      </p>,
    );
    await userEvent.click(screen.getByRole("button", { name: "Copy factoryd doctor -fix" }));
    expect(writeText).toHaveBeenCalledExactlyOnceWith("factoryd doctor -fix");
    await userEvent.click(screen.getByRole("button", { name: "Copy factoryd worker" }));
    expect(writeText).toHaveBeenLastCalledWith("factoryd worker");
    expect(screen.getAllByRole("button")).toHaveLength(2);
  });

  test("a long code is named by its first 40 characters but copies whole", async () => {
    const writeText = vi.fn(() => Promise.resolve());
    const long = 'factoryd reject -to plan -reason "what to change" req-0123456789';
    render(
      <p>
        <TextWithCode text={`Run \`${long}\``} writeText={writeText} />
      </p>,
    );
    await userEvent.click(screen.getByRole("button", { name: `Copy ${long.slice(0, 40)}…` }));
    expect(writeText).toHaveBeenCalledExactlyOnceWith(long);
  });

  test("copy={false} draws none", () => {
    render(
      <p>
        <TextWithCode text="Run `factoryd worker`" copy={false} />
      </p>,
    );
    expect(screen.queryByRole("button")).not.toBeInTheDocument();
  });

  test("an unbalanced backtick draws none", () => {
    render(
      <p>
        <TextWithCode text="a ` b" />
      </p>,
    );
    expect(screen.queryByRole("button")).not.toBeInTheDocument();
  });
});

test("a code span with a hidden character or a line break is shown escaped and offers no copy", () => {
  const writeText = vi.fn(() => Promise.resolve());
  const { container } = render(
    <p>
      <TextWithCode
        text={"run `factoryd retry\u200b req-1` or `a\nrm -rf x`, then `factoryd worker`"}
        writeText={writeText}
      />
    </p>,
  );
  const codes = [...container.querySelectorAll("code")].map((c) => c.textContent);
  // The zero-width space is drawn as its escape, not hidden.
  expect(codes[0]).toBe("factoryd retry\\u{200B} req-1");
  expect(codes[2]).toBe("factoryd worker");
  // Only the span that pastes as it reads has a copy button.
  expect(screen.getAllByRole("button").map((b) => b.getAttribute("aria-label"))).toEqual([
    "Copy factoryd worker",
  ]);
});
