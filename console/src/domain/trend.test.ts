import { asObject } from "@/domain/decode";
import { bucketLabel, decodeProjectTrend, medianText, shareText } from "@/domain/trend";
import { readFixtureJson } from "@/test/fixtures";

const at = "GET /projects/{project}/trend";

test("the contract fixture decodes", () => {
  const trend = decodeProjectTrend(asObject(readFixtureJson("api/project-trend.json"), at), at);
  expect(trend.project).toBe("app");
  expect(trend.bucketDays).toBe(7);
  expect(trend.overall.tickets).toBe(3);
  expect(trend.overall.oneShotRate).toBeCloseTo(0.3333);
  expect(trend.overall.quarantinedBy.map((c) => c.name)).toEqual([
    "canonical_verify",
    "tests_added",
  ]);
  expect(trend.overall.spendCostMicroUsd).toBeGreaterThan(0);
  expect(trend.buckets.length).toBeGreaterThan(0);
  expect(bucketLabel(trend.buckets[0]!)).toMatch(/^\d{4}-\d{2}-\d{2}$/);
});

test("an empty report decodes: null rates, empty lists", () => {
  const empty = {
    tickets: 0,
    one_shot_rate: null,
    accepted_rate: null,
    rounds_to_green: { series: 0, median: 0, p90: 0 },
    quarantined_by: [],
    halted_by: [],
    corrective_builds: { ran: 0, accepted: 0 },
    spend: { tokens: 0, cost_micro_usd: 0, per_accepted_ticket_micro_usd: 0 },
  };
  const trend = decodeProjectTrend(
    { project: "app", bucket_days: 7, overall: empty, buckets: [] },
    at,
  );
  expect(trend.overall.oneShotRate).toBeNull();
  expect(trend.buckets).toEqual([]);
  expect(medianText(trend.overall.roundsToGreen)).toBe("-");
});

test("a response without the overall block is refused with the route's name", () => {
  expect(() => decodeProjectTrend({ project: "app", bucket_days: 7 }, at)).toThrow(
    /GET \/projects\/\{project\}\/trend/,
  );
});

test("shares print as the CLI prints them", () => {
  expect(shareText(3, 8)).toBe("3/8 (38%)");
  expect(shareText(0, 0)).toBe("-");
  expect(shareText(4, 4)).toBe("4/4 (100%)");
});
