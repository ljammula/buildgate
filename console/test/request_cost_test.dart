import 'dart:convert';
import 'dart:io';

import 'package:console/models.dart';
import 'package:console/request_cost.dart';
import 'package:flutter_test/flutter_test.dart';

RequestSummary _summary({
  SpecEvidence? specEvidence,
  PlanEvidence? planEvidence,
}) => RequestSummary(
  id: 'r1',
  workspace: '/repos/app',
  project: 'app',
  state: 'building',
  submittedAt: '2026-09-10T08:00:00Z',
  updatedAt: '2026-09-10T09:00:00Z',
  specEvidence: specEvidence,
  planEvidence: planEvidence,
);

void main() {
  group('requestTokenTotal', () {
    test('is null when neither spec nor plan evidence is present', () {
      expect(requestTokenTotal(_summary()), isNull);
    });

    test('is null when evidence is present but usage itself is null '
        '(SpecEvidence.Usage preserves JSON null)', () {
      expect(
        requestTokenTotal(
          _summary(specEvidence: const SpecEvidence(usage: null)),
        ),
        isNull,
      );
    });

    test('reads totalTokens when present', () {
      final summary = _summary(
        specEvidence: SpecEvidence(
          usage: Usage(fields: const {'totalTokens': 1200}),
        ),
      );
      expect(requestTokenTotal(summary), 1200);
    });

    test('falls back to input + output when totalTokens is absent', () {
      final summary = _summary(
        specEvidence: SpecEvidence(
          usage: Usage(fields: const {'input': 100, 'output': 50}),
        ),
      );
      expect(requestTokenTotal(summary), 150);
    });

    test('sums spec and plan evidence together', () {
      final summary = _summary(
        specEvidence: SpecEvidence(
          usage: Usage(fields: const {'totalTokens': 1000}),
        ),
        planEvidence: PlanEvidence(
          usage: Usage(fields: const {'totalTokens': 500}),
        ),
      );
      expect(requestTokenTotal(summary), 1500);
    });
  });

  group('formatRequestTokenTotal', () {
    test('missing evidence renders an em dash, never zero', () {
      expect(formatRequestTokenTotal(null), '—');
    });

    test('renders small totals as a plain count', () {
      expect(formatRequestTokenTotal(0), '0 tok');
      expect(formatRequestTokenTotal(500), '500 tok');
    });

    test('renders large totals compactly in thousands', () {
      expect(formatRequestTokenTotal(12400), '12k tok');
      expect(formatRequestTokenTotal(1500), '1.5k tok');
    });
  });

  group('formatTokenCount', () {
    test('renders small counts as a plain number', () {
      expect(formatTokenCount(823), '823');
    });

    test('renders thousands compactly', () {
      expect(formatTokenCount(12300), '12.3k');
    });

    test('renders millions compactly', () {
      expect(formatTokenCount(1200000), '1.2M');
    });
  });

  group('formatModelUsageLine', () {
    test('renders a known model id and its token count', () {
      expect(
        formatModelUsageLine(
          const ModelUsage(model: 'gpt-5.6-luna', tokens: 478300),
        ),
        'gpt-5.6-luna · 478.3k tokens',
      );
    });

    test('renders "unknown model" for an empty model id', () {
      expect(
        formatModelUsageLine(const ModelUsage(model: '', tokens: 12300)),
        'unknown model · 12.3k tokens',
      );
    });

    test('prefixes the token count with "≥ " when incomplete', () {
      expect(
        formatModelUsageLine(
          const ModelUsage(model: 'gpt-5.6-luna', tokens: 478300),
          complete: false,
        ),
        'gpt-5.6-luna · ≥ 478.3k tokens',
      );
    });
  });

  group('formatUsageSummary', () {
    CostSummary summary({
      List<ModelUsage> byModel = const [],
      int? tokens,
      bool tokensComplete = true,
    }) => CostSummary(
      spec: 0,
      plan: 0,
      runs: 0,
      total: 0,
      currency: 'usd',
      complete: true,
      tokens: tokens,
      tokensComplete: tokensComplete,
      byModel: byModel,
    );

    test('renders an em dash when nothing is known', () {
      expect(formatUsageSummary(summary()), '—');
    });

    test('falls back to a bare token count when byModel is empty', () {
      expect(formatUsageSummary(summary(tokens: 478300)), '478.3k tokens');
    });

    test('renders a single model line', () {
      expect(
        formatUsageSummary(
          summary(
            byModel: const [ModelUsage(model: 'gpt-5.6-luna', tokens: 478300)],
            tokens: 478300,
          ),
        ),
        'gpt-5.6-luna · 478.3k tokens',
      );
    });

    test('appends "+N more" for several models', () {
      expect(
        formatUsageSummary(
          summary(
            byModel: const [
              ModelUsage(model: 'gpt-5.6-luna', tokens: 478300),
              ModelUsage(model: 'claude-sonnet-5', tokens: 1000),
            ],
            tokens: 479300,
          ),
        ),
        'gpt-5.6-luna +1 more · 479.3k tokens',
      );
    });

    test('prefixes with "≥ " when tokensComplete is false', () {
      expect(
        formatUsageSummary(summary(tokens: 500, tokensComplete: false)),
        '≥ 500 tokens',
      );
    });
  });

  group('formatModelUsageDetailLine', () {
    test('matches formatModelUsageLine when role/cost are absent (an '
        'older evidence record)', () {
      expect(
        formatModelUsageDetailLine(
          const ModelUsage(model: 'gpt-5.6-luna', tokens: 478300),
        ),
        'gpt-5.6-luna · 478.3k tokens',
      );
    });

    test('prefixes the role when present', () {
      expect(
        formatModelUsageDetailLine(
          const ModelUsage(
            role: 'planning',
            model: 'gpt-5.6-luna',
            tokens: 150,
          ),
        ),
        'planning · gpt-5.6-luna · 150 tokens',
      );
    });

    test('appends the dollar cost when costMicroUsd is nonzero', () {
      expect(
        formatModelUsageDetailLine(
          const ModelUsage(
            role: 'execution',
            model: 'gpt-5.6-luna',
            tokens: 150,
            costMicroUsd: 1500000,
          ),
        ),
        'execution · gpt-5.6-luna · 150 tokens · \$1.50',
      );
    });

    test('omits the cost suffix when costMicroUsd is 0', () {
      expect(
        formatModelUsageDetailLine(
          const ModelUsage(role: 'review', model: 'gpt-5.6-luna', tokens: 10),
        ),
        'review · gpt-5.6-luna · 10 tokens',
      );
    });
  });

  group('formatCostPerAcceptedTicket', () {
    CostSummary summary({
      int acceptedTickets = 0,
      int costPerAcceptedTicketMicroUsd = 0,
    }) => CostSummary(
      spec: 0,
      plan: 0,
      runs: 0,
      total: 0,
      currency: 'usd',
      complete: true,
      acceptedTickets: acceptedTickets,
      costPerAcceptedTicketMicroUsd: costPerAcceptedTicketMicroUsd,
    );

    test('is null when acceptedTickets is 0', () {
      expect(formatCostPerAcceptedTicket(summary()), isNull);
    });

    test('renders the dollar figure and ticket count when accepted', () {
      expect(
        formatCostPerAcceptedTicket(
          summary(acceptedTickets: 2, costPerAcceptedTicketMicroUsd: 1750000),
        ),
        '\$1.75 / accepted ticket (2 accepted tickets)',
      );
    });

    test('uses singular wording for exactly one accepted ticket', () {
      expect(
        formatCostPerAcceptedTicket(
          summary(acceptedTickets: 1, costPerAcceptedTicketMicroUsd: 500000),
        ),
        '\$0.50 / accepted ticket (1 accepted ticket)',
      );
    });
  });

  group('formatUsageLines', () {
    test('renders one line per model', () {
      final summary = CostSummary(
        spec: 0,
        plan: 0,
        runs: 0,
        total: 0,
        currency: 'usd',
        complete: true,
        tokensComplete: true,
        byModel: const [
          ModelUsage(model: 'gpt-5.6-luna', tokens: 478300),
          ModelUsage(model: '', tokens: 100),
        ],
      );
      expect(formatUsageLines(summary), [
        'gpt-5.6-luna · 478.3k tokens',
        'unknown model · 100 tokens',
      ]);
    });

    test('includes role and cost per row when present', () {
      final summary = CostSummary(
        spec: 0,
        plan: 0,
        runs: 0,
        total: 0,
        currency: 'usd',
        complete: true,
        tokensComplete: true,
        byModel: const [
          ModelUsage(
            role: 'planning',
            model: 'gpt-5.6-luna',
            tokens: 150,
            costMicroUsd: 1000000,
          ),
          ModelUsage(
            role: 'execution',
            model: 'gpt-5.6-luna',
            tokens: 15,
            costMicroUsd: 200000,
          ),
        ],
      );
      expect(formatUsageLines(summary), [
        'planning · gpt-5.6-luna · 150 tokens · \$1.00',
        'execution · gpt-5.6-luna · 15 tokens · \$0.20',
      ]);
    });

    test('appends the cost-per-accepted-ticket line when accepted tickets '
        'exist', () {
      final summary = CostSummary(
        spec: 0,
        plan: 0,
        runs: 0,
        total: 0,
        currency: 'usd',
        complete: true,
        tokensComplete: true,
        byModel: const [ModelUsage(model: 'gpt-5.6-luna', tokens: 100)],
        acceptedTickets: 1,
        costPerAcceptedTicketMicroUsd: 900000,
      );
      expect(formatUsageLines(summary), [
        'gpt-5.6-luna · 100 tokens',
        '\$0.90 / accepted ticket (1 accepted ticket)',
      ]);
    });

    test('renders an em dash when nothing is known', () {
      const summary = CostSummary(
        spec: 0,
        plan: 0,
        runs: 0,
        total: 0,
        currency: 'usd',
        complete: true,
      );
      expect(formatUsageLines(summary), ['—']);
    });
  });

  group('formatMedianAcceptedTokens', () {
    test('renders an em dash for no data', () {
      expect(formatMedianAcceptedTokens(null), '—');
    });

    test('renders a formatted token count', () {
      expect(formatMedianAcceptedTokens(478300), '478.3k tokens');
    });
  });

  test('Usage.totalTokens counts cache tokens, ignores non-numeric values, '
      'and ignores a totalTokens below output', () {
    expect(
      const Usage(
        fields: {'input': 10, 'output': 5, 'cacheRead': 100, 'cacheWrite': 1},
      ).totalTokens,
      116,
    );
    expect(
      const Usage(
        fields: {'input': 10, 'output': 5, 'cacheRead': 'x'},
      ).totalTokens,
      15,
    );
    expect(
      const Usage(
        fields: {'totalTokens': 3, 'input': 10, 'output': 5},
      ).totalTokens,
      15,
    );
    expect(
      const Usage(fields: {'totalTokens': 42, 'output': 5}).totalTokens,
      42,
    );
    expect(const Usage(fields: {'note': 'none'}).totalTokens, isNull);
  });

  // The same file cmd/factoryd's and internal/api's golden-vector tests
  // read: the console and the CLI format one token count the same way, and
  // every usage line here is rendered from a cost_summary in the API's own
  // shape.
  group('golden vectors (test/fixtures/vectors/cost.json)', () {
    final vectors =
        jsonDecode(File('test/fixtures/vectors/cost.json').readAsStringSync())
            as Map<String, dynamic>;
    List<Map<String, dynamic>> section(String name) =>
        (vectors[name] as List).cast<Map<String, dynamic>>();

    test('formatTokenCount', () {
      for (final v in section('format_token_count')) {
        expect(formatTokenCount(v['n'] as int), v['want'], reason: '${v['n']}');
      }
    });

    test('formatRequestTokenTotal', () {
      for (final v in section('format_request_token_total')) {
        expect(
          formatRequestTokenTotal(v['tokens'] as int?),
          v['want'],
          reason: '${v['tokens']}',
        );
      }
    });

    test('formatMedianAcceptedTokens', () {
      for (final v in section('format_median_accepted_tokens')) {
        expect(
          formatMedianAcceptedTokens(v['tokens'] as int?),
          v['want'],
          reason: '${v['tokens']}',
        );
      }
    });

    for (final v in section('usage')) {
      test('usage: ${v['name']}', () {
        final summary = CostSummary.fromJson(
          v['cost_summary'] as Map<String, dynamic>,
        );
        expect(formatUsageSummary(summary), v['summary']);
        expect(formatUsageLines(summary), v['lines']);
        expect(formatCostPerAcceptedTicket(summary), v['per_accepted_ticket']);
      });
    }
  });
}
