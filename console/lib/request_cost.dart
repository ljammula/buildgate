import 'models.dart';

/// The inline "token total" figure on a request row.
///
/// SpecEvidence.Usage/PlanEvidence.Usage could be read as a dollar figure
/// ("why is this request at $14 and still in planning?"), but no dollar
/// amount is ever actually present in this data: reading agent/pi/
/// scripts/build_app.py's own parse_usage (what actually populates
/// Usage) shows it sums only numeric top-level fields from a real
/// capture -- `input`/`output`/`cacheRead`/`cacheWrite`/`reasoning`/
/// `totalTokens` -- and its own doc comment records that the nested
/// `cost` sub-object is deliberately excluded from that sum (all-zero
/// against every real, uncosted local-model capture it was checked
/// against). Per-run dollar cost does exist elsewhere
/// (`Run.AgentEvidenceRound`'s relay-consumed-cost fields, and
/// `ProjectStats.medianAcceptedCostMicroUsd` computed from those
/// server-side) but that is run-level evidence a request-level view
/// cannot reach without a per-ticket run join this phase deliberately
/// does not do (deferred to a later server-side rollup).
///
/// So this renders a **token total**, not a dollar figure -- the only
/// value actually available from the evidence fields -- as a cost proxy
/// inline on the row, so an operator doesn't need to leave the board to
/// see it, without inventing a $0.00 that was never computed from real
/// evidence.
int? requestTokenTotal(RequestSummary request) {
  final specTotal = request.specEvidence?.usage?.totalTokens;
  final planTotal = request.planEvidence?.usage?.totalTokens;
  if (specTotal == null && planTotal == null) return null;
  return (specTotal ?? 0) + (planTotal ?? 0);
}

/// Formats [requestTokenTotal]'s result for display. Missing evidence
/// (both spec and plan usage absent, e.g. a request still in
/// spec_drafting) renders as an em dash -- never `0`, which would read as
/// "confirmed zero tokens" rather than "not known yet".
String formatRequestTokenTotal(int? tokens) {
  if (tokens == null) return '—';
  if (tokens >= 1000) {
    final thousands = tokens / 1000;
    return '${thousands.toStringAsFixed(thousands < 10 ? 1 : 0)}k tok';
  }
  return '$tokens tok';
}

/// Shared token-count formatter ("823", "12.3k", "1.2M") -- moved here from
/// run_detail_screen.dart (as `_formatTokenCount`) so every usage renderer
/// in this file and the run detail screen format token counts identically.
/// Mirrors cmd/factoryd's own formatTokenCount (round_summary.go).
String formatTokenCount(int n) {
  if (n >= 1000000) return '${(n / 1000000).toStringAsFixed(1)}M';
  if (n >= 1000) return '${(n / 1000).toStringAsFixed(1)}k';
  return '$n';
}

/// Formats one [ModelUsage] entry as "gpt-5.6-luna · 478.3k tokens", or
/// "unknown model · 12.3k tokens" when [usage.model] is empty (the
/// contributing evidence recorded no model id). [complete] false prefixes
/// the token count with "≥ " -- the containing summary's tokens are a
/// lower bound, not an exact figure.
String formatModelUsageLine(ModelUsage usage, {bool complete = true}) {
  final model = usage.model.isEmpty ? 'unknown model' : usage.model;
  final prefix = complete ? '' : '≥ ';
  return '$model · $prefix${formatTokenCount(usage.tokens)} tokens';
}

/// Formats a server-computed [CostSummary]'s usage for a single-line
/// context (the request list row): the model's line when one model was
/// used; with several, "first-model +N more · <total> tokens", so the row
/// never shows only the first model's share of the spend. Falls back to a bare
/// token count when [summary.byModel] is empty but [summary.tokens] is
/// known (an older evidence shape with no model id recorded anywhere),
/// and "—" when nothing is known at all -- the operator explicitly does
/// not want a dollar figure here, only model id and tokens spent.
String formatUsageSummary(CostSummary summary) {
  if (summary.byModel.isEmpty) {
    final tokens = summary.tokens;
    if (tokens == null) return '—';
    final prefix = summary.tokensComplete ? '' : '≥ ';
    return '$prefix${formatTokenCount(tokens)} tokens';
  }
  final first = formatModelUsageLine(
    summary.byModel.first,
    complete: summary.tokensComplete,
  );
  if (summary.byModel.length == 1) return first;
  final prefix = summary.tokensComplete ? '' : '≥ ';
  final model = summary.byModel.first.model.isEmpty
      ? 'unknown model'
      : summary.byModel.first.model;
  final total =
      summary.tokens ??
      summary.byModel.fold<int>(0, (sum, m) => sum + m.tokens);
  return '$model +${summary.byModel.length - 1} more · '
      '$prefix${formatTokenCount(total)} tokens';
}

/// formatModelUsageDetailLine extends [formatModelUsageLine] with M3-C1's
/// own role + cost breakdown: "planning · gpt-5.6-luna · 150 tokens ·
/// $1.50" when [usage.role]/[usage.costMicroUsd] are populated. Falls back
/// to [formatModelUsageLine]'s own bare "model · tokens" text when both are
/// absent (an older evidence record, or an attempt whose Kind never
/// resolves a role -- see [ModelUsage.role]'s own doc comment), so a
/// pre-M3-C1 evidence record renders exactly as it always did. Used by
/// [formatUsageLines] (the triage/approve-confirm "Usage so far" lines),
/// never by run_detail_screen.dart's own bare-token rendering (a separate,
/// out-of-scope screen this PR does not touch).
String formatModelUsageDetailLine(ModelUsage usage, {bool complete = true}) {
  final base = formatModelUsageLine(usage, complete: complete);
  final rolePrefix = usage.role.isEmpty ? '' : '${usage.role} · ';
  final costSuffix = usage.costMicroUsd > 0
      ? ' · \$${(usage.costMicroUsd / 1e6).toStringAsFixed(2)}'
      : '';
  return '$rolePrefix$base$costSuffix';
}

/// Formats the "cost per accepted ticket" line from a [CostSummary]'s own
/// acceptedTickets/costPerAcceptedTicketMicroUsd (api.CostSummary's own
/// fields) -- null when acceptedTickets is 0, so a caller renders nothing
/// rather than a misleading "$0.00 per ticket" for a request with nothing
/// accepted yet, or one computed by an older server that predates these
/// fields (both default to 0 the same way).
String? formatCostPerAcceptedTicket(CostSummary summary) {
  if (summary.acceptedTickets <= 0) return null;
  final dollars = summary.costPerAcceptedTicketMicroUsd / 1e6;
  final tickets = summary.acceptedTickets == 1
      ? '1 accepted ticket'
      : '${summary.acceptedTickets} accepted tickets';
  return '\$${dollars.toStringAsFixed(2)} / accepted ticket ($tickets)';
}

/// Formats every model line of a [CostSummary] for a full-detail context
/// (triage, the approve-confirm dialog) -- one line per [ModelUsage]
/// entry (role + cost included via [formatModelUsageDetailLine]), or a
/// single fallback line when nothing recorded a model id, plus a trailing
/// "cost per accepted ticket" line ([formatCostPerAcceptedTicket]) when
/// this request has at least one accepted ticket.
List<String> formatUsageLines(CostSummary summary) {
  final lines = <String>[];
  if (summary.byModel.isEmpty) {
    final tokens = summary.tokens;
    if (tokens == null) {
      lines.add('—');
    } else {
      final prefix = summary.tokensComplete ? '' : '≥ ';
      lines.add('$prefix${formatTokenCount(tokens)} tokens');
    }
  } else {
    for (final usage in summary.byModel) {
      lines.add(
        formatModelUsageDetailLine(usage, complete: summary.tokensComplete),
      );
    }
  }
  final perTicket = formatCostPerAcceptedTicket(summary);
  if (perTicket != null) lines.add(perTicket);
  return lines;
}

/// Formats a project's median accepted tokens (ProjectStats.
/// medianAcceptedTokens) for display, e.g. "478.3k tokens" -- null (no
/// accepted runs yet) renders as "—". Shared by project_stats_screen.dart
/// and ops_screen.dart so the two never disagree about wording.
String formatMedianAcceptedTokens(int? tokens) {
  if (tokens == null) return '—';
  return '${formatTokenCount(tokens)} tokens';
}
