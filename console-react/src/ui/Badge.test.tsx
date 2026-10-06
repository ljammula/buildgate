import { render, screen } from "@testing-library/react";

import { Badge } from "@/ui/Badge";
import { toneClasses } from "@/ui/tone";

test("renders its text and takes its colours from the tone table", () => {
  render(<Badge tone="danger">Failed</Badge>);
  const badge = screen.getByText("Failed");
  for (const cls of Object.values(toneClasses.danger)) {
    expect(badge).toHaveClass(cls);
  }
});

test("defaults to neutral and the outline variant has no soft fill", () => {
  render(
    <>
      <Badge>Plain</Badge>
      <Badge variant="outline" tone="success">
        Outlined
      </Badge>
    </>,
  );
  expect(screen.getByText("Plain")).toHaveClass(toneClasses.neutral.soft);
  expect(screen.getByText("Outlined")).not.toHaveClass(toneClasses.success.soft);
});
