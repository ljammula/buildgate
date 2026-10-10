import { asObject } from "@/domain/decode";
import {
  costPerAcceptedText,
  decodeFactoryStats,
  numbersRows,
  rateText,
  spendText,
  statsEmpty,
} from "@/domain/stats";
import { readFixtureJson } from "@/test/fixtures";

const at = "GET /stats";
const fixture = () => decodeFactoryStats(asObject(readFixtureJson("api/stats.json"), at), at);

const metrics = (extra: Record<string, unknown> = {}) => ({
  tickets: 0,
  one_shot_rate: null,
  accepted_rate: null,
  rounds_to_green: { series: 0, median: 0, p90: 0 },
  quarantined_by: [],
  halted_by: [],
  corrective_builds: { ran: 0, accepted: 0 },
  spend: { tokens: 0, cost_micro_usd: 0, per_accepted_ticket_micro_usd: 0 },
  ...extra,
});
const report = (project: string, overall: Record<string, unknown>) => ({
  project,
  bucket_days: 7,
  overall,
  buckets: [],
});

test("the contract fixture decodes: one report overall, one per project", () => {
  const stats = fixture();
  expect(stats.overall.project).toBe("");
  expect(stats.overall.overall.tickets).toBe(3);
  expect(stats.projects.map((p) => p.project)).toEqual(["app"]);
  expect(stats.projects[0]?.overall.acceptedRate).toBeCloseTo(0.6667);
  expect(stats.overall.overall.perAcceptedTicketMicroUsd).toBe(750001);
});

test("a body without `overall` is refused with the route's name", () => {
  expect(() => decodeFactoryStats({ projects: [] }, at)).toThrow("GET /stats");
});

test("the fixture's rows: overall first, then each project", () => {
  expect(numbersRows(fixture())).toEqual([
    {
      label: "Overall",
      tickets: "3",
      oneShot: "1/3 (33%)",
      accepted: "2/3 (67%)",
      medianRounds: "1",
      topQuarantine: "canonical_verify (1)",
      spend: "478.3k tokens · $1.50",
      costPerAccepted: "$0.75",
    },
    {
      label: "app",
      tickets: "3",
      oneShot: "1/3 (33%)",
      accepted: "2/3 (67%)",
      medianRounds: "1",
      topQuarantine: "canonical_verify (1)",
      spend: "478.3k tokens · $1.50",
      costPerAccepted: "$0.75",
    },
  ]);
});

test("null rates and nothing spent read as -, never as 0%", () => {
  const stats = decodeFactoryStats(
    { overall: report("", metrics()), projects: [report("idle", metrics())] },
    at,
  );
  expect(numbersRows(stats)[1]).toEqual({
    label: "idle",
    tickets: "0",
    oneShot: "-",
    accepted: "-",
    medianRounds: "-",
    topQuarantine: "-",
    spend: "-",
    costPerAccepted: "-",
  });
  expect(statsEmpty(stats)).toBe(false);
});

test("an empty data dir has nothing to show", () => {
  const stats = decodeFactoryStats({ overall: report("", metrics()), projects: [] }, at);
  expect(statsEmpty(stats)).toBe(true);
  expect(statsEmpty(fixture())).toBe(false);
});

test("rateText uses the server's rate", () => {
  expect(rateText(0, 4, 0)).toBe("0/4 (0%)");
  expect(rateText(1, 8, 0.125)).toBe("1/8 (13%)");
  expect(rateText(0, 0, null)).toBe("-");
});

test("spend names dollars only when a cost was recorded", () => {
  const m = (spend: Record<string, number>, accepted = 0) =>
    decodeFactoryStats({ overall: report("", metrics({ accepted, spend })), projects: [] }, at)
      .overall.overall;
  expect(spendText(m({ tokens: 12300, cost_micro_usd: 0 }))).toBe("12.3k tokens");
  expect(spendText(m({ tokens: 950, cost_micro_usd: 40000 }))).toBe("950 tokens · $0.04");
  // Spend with nothing accepted has no per-ticket figure.
  expect(costPerAcceptedText(m({ tokens: 950, cost_micro_usd: 40000 }))).toBe("-");
  expect(
    costPerAcceptedText(
      m({ tokens: 1, cost_micro_usd: 2, per_accepted_ticket_micro_usd: 2500000 }, 2),
    ),
  ).toBe("$2.50");
});
