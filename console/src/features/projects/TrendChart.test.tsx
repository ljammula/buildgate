import { render, screen } from "@testing-library/react";

import { asObject } from "@/domain/decode";
import { type ProjectTrend, decodeProjectTrend } from "@/domain/trend";
import { readFixtureJson } from "@/test/fixtures";

import { TrendChart } from "./TrendChart";

const at = "GET /projects/{project}/trend";

/** A trend whose buckets are weeks from 2026-09-06, one per `[oneShot, tickets]`. */
function trendOf(weeks: readonly (readonly [number, number])[], bucketDays = 7): ProjectTrend {
  const base = decodeProjectTrend(asObject(readFixtureJson("api/project-trend.json"), at), at);
  const metrics = base.overall;
  return {
    ...base,
    bucketDays,
    buckets: weeks.map(([oneShot, tickets], i) => ({
      start: `2026-09-${String(6 + 7 * i).padStart(2, "0")}T00:00:00Z`,
      end: "",
      metrics: {
        ...metrics,
        tickets,
        oneShot,
        oneShotRate: tickets === 0 ? null : oneShot / tickets,
      },
    })),
  };
}

function line(): string {
  return screen.getByRole("img").querySelector("path")?.getAttribute("d") ?? "";
}

test("several weeks are one line on a 0-100% axis, named by a sentence", () => {
  render(
    <TrendChart
      trend={trendOf([
        [1, 4],
        [2, 4],
        [3, 4],
      ])}
    />,
  );
  expect(
    screen.getByRole("img", {
      name: "One-shot acceptance rate: 25% (1/4 tickets) in the week of 2026-09-06, 75% (3/4 tickets) in the week of 2026-09-20. Lowest 25%, highest 75%.",
    }),
  ).toBeInTheDocument();
  // 25%, 50%, 75% of a plot 118 high from y=10: evenly spaced left to right.
  expect(line()).toBe("M34.0 98.5L330.0 69.0L626.0 39.5");
  expect(screen.getAllByTestId("trend-chart-point")).toHaveLength(3);
  expect(screen.getByText("50% (2/4 tickets) in the week of 2026-09-13")).toBeInTheDocument();
  expect(screen.getByText("One-shot rate per week")).toBeInTheDocument();
  for (const label of ["0%", "50%", "100%", "2026-09-06", "2026-09-20"]) {
    expect(screen.getByText(label)).toBeInTheDocument();
  }
});

test("a week with no ticket is a gap in the line, never a point at 0%", () => {
  render(
    <TrendChart
      trend={trendOf([
        [0, 2],
        [0, 0],
        [4, 4],
        [2, 4],
      ])}
    />,
  );
  // The first week is a real 0% (y=128); the second has no point; the line starts again.
  expect(line()).toBe("M34.0 128.0M428.7 10.0L626.0 69.0");
  expect(screen.getAllByTestId("trend-chart-point")).toHaveLength(3);
  expect(screen.getByRole("img")).toHaveAccessibleName(
    "One-shot acceptance rate: 0% (0/2 tickets) in the week of 2026-09-06, 50% (2/4 tickets) in the week of 2026-09-27. Lowest 0%, highest 100%. 1 of 4 periods had no ticket.",
  );
});

test("one week is one point in the middle, and no week with a ticket draws nothing", () => {
  const one = render(<TrendChart trend={trendOf([[3, 8]], 14)} />);
  expect(screen.getByRole("img")).toHaveAccessibleName(
    "One-shot acceptance rate: 38% (3/8 tickets) in the 14 days from 2026-09-06.",
  );
  expect(line()).toBe("M330.0 83.8");
  expect(screen.getAllByTestId("trend-chart-point")).toHaveLength(1);
  expect(screen.getByText("One-shot rate per 14 days")).toBeInTheDocument();
  one.unmount();

  const none = render(<TrendChart trend={trendOf([])} />);
  expect(screen.queryByTestId("trend-chart")).toBeNull();
  none.unmount();
  render(<TrendChart trend={trendOf([[0, 0]])} />);
  expect(screen.queryByTestId("trend-chart")).toBeNull();
});

test("the colours are theme tokens, so the chart follows light and dark", () => {
  const { container } = render(<TrendChart trend={trendOf([[1, 2]])} />);
  const painted = [...container.querySelectorAll("*")].map((el) => [
    el.getAttribute("fill"),
    el.getAttribute("stroke"),
    el.getAttribute("style"),
  ]);
  // No colour is written on an element: only "none"/"transparent", the rest by token class.
  expect(new Set(painted.flat())).toEqual(new Set([null, "none", "transparent"]));
  expect(
    [...container.querySelectorAll("[class]")].map((el) => el.getAttribute("class")),
  ).not.toContainEqual(expect.stringMatching(/dark:|zinc|slate|\[#/));
  expect(container.querySelector("path")).toHaveClass("stroke-accent");
});
