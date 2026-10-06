import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { DigestedText } from "@/shared/request/DigestedText";

describe("DigestedText", () => {
  test("a short message is shown as it is, with no disclosure", () => {
    render(<DigestedText text="gh: not logged in" />);

    expect(screen.getByText("gh: not logged in")).toBeInTheDocument();
    expect(screen.queryByRole("heading")).not.toBeInTheDocument();
  });

  test("a long message shows its first sentence and keeps the whole one a click away", async () => {
    const whole = "The pull request could not be opened. Run factoryd retry with pull requests on.";
    render(<DigestedText text={whole} fullLabel="Full error" />);

    expect(screen.getByText("The pull request could not be opened.")).toBeInTheDocument();
    const details = screen.getByRole("heading", { name: "Full error" }).closest("details");
    expect(details?.open).toBe(false);

    await userEvent.click(screen.getByRole("heading", { name: "Full error" }));

    expect(details?.open).toBe(true);
    expect(screen.getByText(whole)).toBeVisible();
  });
});
