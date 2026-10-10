import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { ApiError } from "@/domain/apiError";
import { StaleWarning } from "@/ui/StaleWarning";

const boom = new ApiError(500, JSON.stringify({ error: "disk full" }));

describe("StaleWarning", () => {
  test("says the default message with the error's raw text", () => {
    render(<StaleWarning error={new Error("socket hang up")} />);
    expect(screen.getByRole("status")).toHaveTextContent(
      "Showing the last successfully loaded data. The refresh failed: socket hang up",
    );
    expect(screen.queryByRole("button")).not.toBeInTheDocument();
  });

  test("detail picks the headline or the next step", () => {
    const { rerender } = render(<StaleWarning error={boom} detail="headline" />);
    expect(screen.getByRole("status")).toHaveTextContent("refresh failed: Request failed (500)");
    rerender(<StaleWarning error={new ApiError(404, "gone")} detail="next-step" />);
    expect(screen.getByRole("status")).toHaveTextContent(
      "It may have been pruned, or the id in the URL is wrong.",
    );
  });

  test("startClass names the start-token fix", () => {
    render(<StaleWarning error={new ApiError(401, "no")} detail="next-step" startClass />);
    expect(screen.getByRole("status")).toHaveTextContent("start token");
  });

  test("children replace the message; testId is set", () => {
    render(
      <StaleWarning error={boom} testId="stale-banner">
        Showing the last release state, refresh failed:
      </StaleWarning>,
    );
    expect(screen.getByTestId("stale-banner")).toHaveTextContent(
      "Showing the last release state, refresh failed: disk full",
    );
  });

  test("Retry calls onRetry and is disabled while retrying", async () => {
    const onRetry = vi.fn();
    const { rerender } = render(<StaleWarning error={boom} onRetry={onRetry} />);
    await userEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(onRetry).toHaveBeenCalledOnce();
    rerender(<StaleWarning error={boom} onRetry={onRetry} retrying />);
    expect(screen.getByRole("button", { name: "Retry" })).toBeDisabled();
  });

  test("detail=callout stacks the full error callout over the message", () => {
    render(
      <StaleWarning error={boom} detail="callout" testId="oracle-stale-listing">
        The files below may be out of date.
      </StaleWarning>,
    );
    const box = screen.getByTestId("oracle-stale-listing");
    expect(box).toHaveTextContent("Request failed (500)");
    expect(box).toHaveTextContent("The files below may be out of date.");
    expect(screen.getByRole("alert")).toBeInTheDocument();
  });
});
