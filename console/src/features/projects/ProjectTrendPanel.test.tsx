import { screen, within } from "@testing-library/react";

import { readFixtureJson } from "@/test/fixtures";
import { apiErrorResponse, json, renderApp } from "@/test/render";

import { ProjectTrendPanel } from "./ProjectTrendPanel";

function renderTrend(reply: () => Response) {
  return renderApp(<ProjectTrendPanel project="app" />, {
    path: "/app/projects",
    pattern: "/app/projects",
    server: [{ on: "GET /projects/app/trend", reply }],
  });
}

const metrics = (over: Record<string, unknown> = {}) => ({
  tickets: 8,
  one_shot: 3,
  one_shot_rate: 0.375,
  accepted: 6,
  accepted_rate: 0.75,
  rounds_to_green: { series: 6, median: 2, p90: 4 },
  failed_round_pairs: 5,
  comparable_pairs: 4,
  same_failure_pairs: 1,
  no_change_pairs: 1,
  quarantined_by: [{ name: "tests_added <b>x</b>", runs: 3 }],
  halted_by: [],
  corrective_builds: { ran: 2, accepted: 1 },
  spend: {
    tokens: 10,
    cost_micro_usd: 1000,
    runs_with_spend: 1,
    per_accepted_ticket_micro_usd: 166,
  },
  ...over,
});

const trend = {
  project: "app",
  since: "",
  until: "",
  bucket_days: 7,
  runs: 12,
  unfinished: 1,
  excluded_runs: 2,
  overall: metrics(),
  buckets: [
    {
      start: "2026-09-27T00:00:00Z",
      end: "2026-10-04T00:00:00Z",
      metrics: metrics({
        tickets: 0,
        one_shot_rate: null,
        accepted_rate: null,
        one_shot: 0,
        accepted: 0,
        comparable_pairs: 0,
        same_failure_pairs: 0,
        rounds_to_green: { series: 0, median: 0, p90: 0 },
      }),
    },
    { start: "2026-10-04T00:00:00Z", end: "2026-10-11T00:00:00Z", metrics: metrics() },
  ],
};

test("the tiles, the bucket table and the two lists show the report as the CLI prints it", async () => {
  renderTrend(() => json(trend));
  const tiles = await screen.findAllByTestId("trend-tile");
  expect(tiles.map((t) => t.textContent)).toEqual([
    "One-shot38%3/8 (38%) tickets built right the first time",
    "Accepted75%6/8 (75%) tickets ended accepted",
    "Rounds to green2median; 90th percentile 4",
    "Same failure twice25%1/4 (25%) failed-round pairs",
  ]);
  const rows = screen.getAllByTestId("trend-bucket");
  expect(rows).toHaveLength(2);
  expect(within(rows[0]!).getByText("2026-09-27")).toBeInTheDocument();
  expect(rows[0]).toHaveTextContent("2026-09-270----");
  expect(rows[1]).toHaveTextContent("2026-10-0483/8 (38%)6/8 (75%)21/4 (25%)");
  expect(screen.getByRole("columnheader", { name: "Week of" })).toBeInTheDocument();
  // The chart sits above the table, which still holds every value.
  const chart = screen.getByRole("img", {
    name: "One-shot acceptance rate: 38% (3/8 tickets) in the week of 2026-10-04. 1 of 2 periods had no ticket.",
  });
  expect(
    chart.compareDocumentPosition(screen.getByRole("table")) & Node.DOCUMENT_POSITION_FOLLOWING,
  ).toBeTruthy();
  const quarantined = screen.getByTestId("trend-quarantined");
  expect(quarantined).toHaveTextContent("tests_added <b>x</b>3");
  expect(quarantined.querySelector("b")).toBeNull();
  expect(screen.getByText("None in this window.")).toBeInTheDocument();
  expect(screen.getByText("2 live-smoke run(s) are not counted.")).toBeInTheDocument();
  expect(screen.getByText("1 run(s) still in progress are not counted.")).toBeInTheDocument();
});

test("the contract fixture renders", async () => {
  renderTrend(() => json(readFixtureJson("api/project-trend.json")));
  expect(await screen.findAllByTestId("trend-tile")).toHaveLength(4);
  expect(screen.getAllByTestId("trend-bucket").length).toBeGreaterThan(0);
  expect(screen.getByRole("img", { name: /^One-shot acceptance rate: / })).toBeInTheDocument();
});

test("a project with no ticket says when the numbers appear", async () => {
  renderTrend(() =>
    json({
      ...trend,
      excluded_runs: 0,
      unfinished: 0,
      overall: metrics({
        tickets: 0,
        one_shot_rate: null,
        accepted_rate: null,
        quarantined_by: [],
      }),
      buckets: [],
    }),
  );
  expect(await screen.findByText("No ticket yet")).toBeInTheDocument();
  expect(screen.queryByTestId("trend-tile")).toBeNull();
});

test("a failed load shows the server's error", async () => {
  renderTrend(() => apiErrorResponse(403, "read endpoint is not authorized"));
  expect(await screen.findByText(/read endpoint is not authorized/)).toBeInTheDocument();
});
