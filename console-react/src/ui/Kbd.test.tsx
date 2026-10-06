import { render, screen } from "@testing-library/react";

import { Kbd } from "@/ui/Kbd";

test("renders the key as text in a kbd element", () => {
  render(<Kbd>Esc</Kbd>);
  const key = screen.getByText("Esc");
  expect(key.tagName).toBe("KBD");
});
