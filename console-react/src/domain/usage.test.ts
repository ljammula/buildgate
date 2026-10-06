import { asObject } from "@/domain/decode";
import { decodeCostSummary, decodeUsage, usageTotalTokens } from "@/domain/usage";
import { readFixtureJson } from "@/test/fixtures";

const total = (fields: Record<string, unknown>) => usageTotalTokens(decodeUsage(fields));

test("usageTotalTokens reads totalTokens when present", () => {
  expect(total({ totalTokens: 1200 })).toBe(1200);
});

test("usageTotalTokens falls back to the parts when totalTokens is absent or below output", () => {
  expect(total({ input: 100, output: 50 })).toBe(150);
  expect(total({ input: 100, output: 50, cacheRead: 10, cacheWrite: 5 })).toBe(165);
  expect(total({ totalTokens: 10, input: 100, output: 50 })).toBe(150);
});

test("usageTotalTokens is null when nothing numeric is present", () => {
  expect(total({})).toBeNull();
  expect(total({ note: "none" })).toBeNull();
  expect(total({ totalTokens: "1200" })).toBeNull();
});

test("every cost_summary of the shared golden vectors decodes", () => {
  const vectors = asObject(readFixtureJson("vectors/cost.json"), "cost.json");
  const usage = vectors.usage as { name: string; cost_summary: unknown }[];
  expect(usage.length).toBeGreaterThan(0);
  for (const v of usage) {
    const summary = decodeCostSummary(asObject(v.cost_summary, v.name), v.name);
    expect(summary.currency).toBe("usd");
  }
  const old = decodeCostSummary(asObject(usage[0]!.cost_summary, "old"), "old");
  expect(old.tokens).toBeNull();
  expect(old.tokensComplete).toBe(false);
});
