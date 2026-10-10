import { render, screen } from "@testing-library/react";

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
