import { type JsonObject, asObject } from "@/domain/decode";
import {
  formatCostPerAcceptedTicket,
  formatMedianAcceptedTokens,
  formatModelUsageDetailLine,
  formatModelUsageLine,
  formatRequestTokenTotal,
  formatTokenCount,
  formatUsageLines,
  formatUsageFigure,
  formatUsageSummary,
  formatUsageTokens,
  requestTokenTotal,
} from "@/domain/cost";
import { type RequestSummary, decodeRequestSummary } from "@/domain/request";
import {
  type CostSummary,
  type ModelUsage,
  decodeCostSummary,
  decodeUsage,
  usageTotalTokens,
} from "@/domain/usage";
import { readFixtureJson } from "@/test/fixtures";

function summaryOf(overrides: JsonObject = {}): RequestSummary {
  return decodeRequestSummary(
    {
      id: "r1",
      workspace: "/repos/app",
      project: "app",
      state: "building",
      submitted_at: "2026-09-10T08:00:00Z",
      updated_at: "2026-09-10T09:00:00Z",
      ...overrides,
    },
    "test",
  );
}

function modelUsage(overrides: Partial<ModelUsage> = {}): ModelUsage {
  return { role: "", model: "", tokens: 0, costMicroUsd: 0, ...overrides };
}

function costSummary(overrides: Partial<CostSummary> = {}): CostSummary {
  return {
    spec: 0,
    plan: 0,
    runs: 0,
    total: 0,
    currency: "usd",
    complete: true,
    subscriptionBilled: false,
    tokens: null,
    tokensComplete: true,
    byModel: [],
    acceptedTickets: 0,
    costPerAcceptedTicketMicroUsd: 0,
    ...overrides,
  };
}

describe("requestTokenTotal", () => {
  test("is null when neither spec nor plan evidence is present", () => {
    expect(requestTokenTotal(summaryOf())).toBeNull();
  });

  test("is null when evidence is present but usage itself is null (SpecEvidence.Usage preserves JSON null)", () => {
    expect(requestTokenTotal(summaryOf({ spec_evidence: { usage: null } }))).toBeNull();
  });

  test("reads totalTokens when present", () => {
    const summary = summaryOf({ spec_evidence: { usage: { totalTokens: 1200 } } });
    expect(requestTokenTotal(summary)).toBe(1200);
  });

  test("falls back to input + output when totalTokens is absent", () => {
    const summary = summaryOf({ spec_evidence: { usage: { input: 100, output: 50 } } });
    expect(requestTokenTotal(summary)).toBe(150);
  });

  test("sums spec and plan evidence together", () => {
    const summary = summaryOf({
      spec_evidence: { usage: { totalTokens: 1000 } },
      plan_evidence: { usage: { totalTokens: 500 } },
    });
    expect(requestTokenTotal(summary)).toBe(1500);
  });
});

describe("formatRequestTokenTotal", () => {
  test("missing evidence renders an em dash, never zero", () => {
    expect(formatRequestTokenTotal(null)).toBe("—");
  });

  test("renders small totals as a plain count", () => {
    expect(formatRequestTokenTotal(0)).toBe("0 tok");
    expect(formatRequestTokenTotal(500)).toBe("500 tok");
  });

  test("renders large totals compactly in thousands", () => {
    expect(formatRequestTokenTotal(12400)).toBe("12k tok");
    expect(formatRequestTokenTotal(1500)).toBe("1.5k tok");
  });
});

describe("formatTokenCount", () => {
  test("renders small counts as a plain number", () => {
    expect(formatTokenCount(823)).toBe("823");
  });

  test("renders thousands compactly", () => {
    expect(formatTokenCount(12300)).toBe("12.3k");
  });

  test("renders millions compactly", () => {
    expect(formatTokenCount(1200000)).toBe("1.2M");
  });
});

describe("formatModelUsageLine", () => {
  test("renders a known model id and its token count", () => {
    expect(formatModelUsageLine(modelUsage({ model: "gpt-5.6-luna", tokens: 478300 }))).toBe(
      "gpt-5.6-luna · 478.3k tokens",
    );
  });

  test('renders "unknown model" for an empty model id', () => {
    expect(formatModelUsageLine(modelUsage({ model: "", tokens: 12300 }))).toBe(
      "unknown model · 12.3k tokens",
    );
  });

  test('prefixes the token count with "≥ " when incomplete', () => {
    expect(formatModelUsageLine(modelUsage({ model: "gpt-5.6-luna", tokens: 478300 }), false)).toBe(
      "gpt-5.6-luna · ≥ 478.3k tokens",
    );
  });
});

describe("formatUsageSummary", () => {
  test("renders an em dash when nothing is known", () => {
    expect(formatUsageSummary(costSummary())).toBe("—");
  });

  test("falls back to a bare token count when byModel is empty", () => {
    expect(formatUsageSummary(costSummary({ tokens: 478300 }))).toBe("478.3k tokens");
  });

  test("renders a single model line", () => {
    expect(
      formatUsageSummary(
        costSummary({
          byModel: [modelUsage({ model: "gpt-5.6-luna", tokens: 478300 })],
          tokens: 478300,
        }),
      ),
    ).toBe("gpt-5.6-luna · 478.3k tokens");
  });

  test('appends "+N more" for several models', () => {
    expect(
      formatUsageSummary(
        costSummary({
          byModel: [
            modelUsage({ model: "gpt-5.6-luna", tokens: 478300 }),
            modelUsage({ model: "claude-sonnet-5", tokens: 1000 }),
          ],
          tokens: 479300,
        }),
      ),
    ).toBe("gpt-5.6-luna +1 more · 479.3k tokens");
  });

  test('prefixes with "≥ " when tokensComplete is false', () => {
    expect(formatUsageSummary(costSummary({ tokens: 500, tokensComplete: false }))).toBe(
      "≥ 500 tokens",
    );
  });
});

describe("formatModelUsageDetailLine", () => {
  test("matches formatModelUsageLine when role/cost are absent (an older evidence record)", () => {
    expect(formatModelUsageDetailLine(modelUsage({ model: "gpt-5.6-luna", tokens: 478300 }))).toBe(
      "gpt-5.6-luna · 478.3k tokens",
    );
  });

  test("prefixes the role when present", () => {
    expect(
      formatModelUsageDetailLine(
        modelUsage({ role: "planning", model: "gpt-5.6-luna", tokens: 150 }),
      ),
    ).toBe("planning · gpt-5.6-luna · 150 tokens");
  });

  test("appends the dollar cost when costMicroUsd is nonzero", () => {
    expect(
      formatModelUsageDetailLine(
        modelUsage({
          role: "execution",
          model: "gpt-5.6-luna",
          tokens: 150,
          costMicroUsd: 1500000,
        }),
      ),
    ).toBe("execution · gpt-5.6-luna · 150 tokens · $1.50");
  });

  test("omits the cost suffix when costMicroUsd is 0", () => {
    expect(
      formatModelUsageDetailLine(modelUsage({ role: "review", model: "gpt-5.6-luna", tokens: 10 })),
    ).toBe("review · gpt-5.6-luna · 10 tokens");
  });
});

describe("formatCostPerAcceptedTicket", () => {
  test("is null when acceptedTickets is 0", () => {
    expect(formatCostPerAcceptedTicket(costSummary())).toBeNull();
  });

  test("renders the dollar figure and ticket count when accepted", () => {
    expect(
      formatCostPerAcceptedTicket(
        costSummary({ acceptedTickets: 2, costPerAcceptedTicketMicroUsd: 1750000 }),
      ),
    ).toBe("$1.75 / accepted ticket (2 accepted tickets)");
  });

  test("uses singular wording for exactly one accepted ticket", () => {
    expect(
      formatCostPerAcceptedTicket(
        costSummary({ acceptedTickets: 1, costPerAcceptedTicketMicroUsd: 500000 }),
      ),
    ).toBe("$0.50 / accepted ticket (1 accepted ticket)");
  });
});

describe("formatUsageLines", () => {
  test("renders one line per model", () => {
    const summary = costSummary({
      byModel: [
        modelUsage({ model: "gpt-5.6-luna", tokens: 478300 }),
        modelUsage({ model: "", tokens: 100 }),
      ],
    });
    expect(formatUsageLines(summary)).toEqual([
      "gpt-5.6-luna · 478.3k tokens",
      "unknown model · 100 tokens",
    ]);
  });

  test("includes role and cost per row when present", () => {
    const summary = costSummary({
      byModel: [
        modelUsage({
          role: "planning",
          model: "gpt-5.6-luna",
          tokens: 150,
          costMicroUsd: 1000000,
        }),
        modelUsage({
          role: "execution",
          model: "gpt-5.6-luna",
          tokens: 15,
          costMicroUsd: 200000,
        }),
      ],
    });
    expect(formatUsageLines(summary)).toEqual([
      "planning · gpt-5.6-luna · 150 tokens · $1.00",
      "execution · gpt-5.6-luna · 15 tokens · $0.20",
    ]);
  });

  test("appends the cost-per-accepted-ticket line when accepted tickets exist", () => {
    const summary = costSummary({
      byModel: [modelUsage({ model: "gpt-5.6-luna", tokens: 100 })],
      acceptedTickets: 1,
      costPerAcceptedTicketMicroUsd: 900000,
    });
    expect(formatUsageLines(summary)).toEqual([
      "gpt-5.6-luna · 100 tokens",
      "$0.90 / accepted ticket (1 accepted ticket)",
    ]);
  });

  test("renders an em dash when nothing is known", () => {
    expect(formatUsageLines(costSummary())).toEqual(["—"]);
  });
});

describe("formatMedianAcceptedTokens", () => {
  test("renders an em dash for no data", () => {
    expect(formatMedianAcceptedTokens(null)).toBe("—");
  });

  test("renders a formatted token count", () => {
    expect(formatMedianAcceptedTokens(478300)).toBe("478.3k tokens");
  });
});

test("Usage.totalTokens counts cache tokens, ignores non-numeric values, and ignores a totalTokens below output", () => {
  const total = (fields: JsonObject) => usageTotalTokens(decodeUsage(fields));
  expect(total({ input: 10, output: 5, cacheRead: 100, cacheWrite: 1 })).toBe(116);
  expect(total({ input: 10, output: 5, cacheRead: "x" })).toBe(15);
  expect(total({ totalTokens: 3, input: 10, output: 5 })).toBe(15);
  expect(total({ totalTokens: 42, output: 5 })).toBe(42);
  expect(total({ note: "none" })).toBeNull();
});

// The same file cmd/factoryd's and internal/api's golden-vector tests read:
// the console and the CLI format one token count the same way, and every
// usage line here is rendered from a cost_summary in the API's own shape.
describe("golden vectors (test/fixtures/vectors/cost.json)", () => {
  const vectors = asObject(readFixtureJson("vectors/cost.json"), "cost.json");
  const section = (name: string): Record<string, unknown>[] =>
    vectors[name] as Record<string, unknown>[];

  test("formatTokenCount", () => {
    const cases = section("format_token_count");
    expect(cases.length).toBeGreaterThan(0);
    for (const v of cases) {
      expect(formatTokenCount(v.n as number), String(v.n)).toBe(v.want);
    }
  });

  test("formatRequestTokenTotal", () => {
    const cases = section("format_request_token_total");
    expect(cases.length).toBeGreaterThan(0);
    for (const v of cases) {
      expect(formatRequestTokenTotal(v.tokens as number | null), String(v.tokens)).toBe(v.want);
    }
  });

  test("formatMedianAcceptedTokens", () => {
    const cases = section("format_median_accepted_tokens");
    expect(cases.length).toBeGreaterThan(0);
    for (const v of cases) {
      expect(formatMedianAcceptedTokens(v.tokens as number | null), String(v.tokens)).toBe(v.want);
    }
  });

  const usageCases = section("usage");
  test("usage cases are present", () => {
    expect(usageCases.length).toBeGreaterThan(0);
  });
  test.each(usageCases.map((v) => [v.name as string, v] as const))("usage: %s", (name, v) => {
    const summary = decodeCostSummary(asObject(v.cost_summary, name), name);
    expect(formatUsageSummary(summary)).toBe(v.summary);
    expect(formatUsageLines(summary)).toEqual(v.lines);
    expect(formatCostPerAcceptedTicket(summary)).toBe(v.per_accepted_ticket);
  });
});

describe("formatUsageTokens and formatUsageFigure", () => {
  test("the token figure is only the number, with no model name", () => {
    const summary = costSummary({
      byModel: [modelUsage({ model: "gpt-5.6-luna", tokens: 478300 })],
      tokens: 478300,
    });
    expect(formatUsageTokens(summary)).toBe("478.3k tokens");
  });

  test("several models add up, and a lower bound is marked", () => {
    const summary = costSummary({
      byModel: [modelUsage({ tokens: 400 }), modelUsage({ tokens: 100 })],
      tokensComplete: false,
    });
    expect(formatUsageTokens(summary)).toBe("≥ 500 tokens");
  });

  test("nothing known is an em dash", () => {
    expect(formatUsageTokens(costSummary())).toBe("—");
  });

  test("the rail figure leads with the dollars when a model recorded a cost", () => {
    const summary = costSummary({
      byModel: [
        modelUsage({ model: "a", tokens: 478300, costMicroUsd: 1_500_000 }),
        modelUsage({ model: "b", tokens: 1200, costMicroUsd: 10_000 }),
      ],
      tokens: 479500,
    });
    expect(formatUsageFigure(summary)).toBe("$1.51 · 479.5k tokens");
  });

  test("without a recorded cost the figure is just the tokens", () => {
    expect(formatUsageFigure(costSummary({ tokens: 1000 }))).toBe("1.0k tokens");
  });
});
