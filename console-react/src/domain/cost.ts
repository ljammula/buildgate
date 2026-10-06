import type { RequestSummary } from "@/domain/request";
import { type CostSummary, type ModelUsage, usageTotalTokens } from "@/domain/usage";

/**
 * The inline "token total" figure on a request row.
 *
 * SpecEvidence.usage/PlanEvidence.usage could be read as a dollar figure
 * ("why is this request at $14 and still in planning?"), but no dollar
 * amount is ever actually present in this data: reading agent/pi/
 * scripts/build_app.py's own parse_usage (what actually populates
 * Usage) shows it sums only numeric top-level fields from a real
 * capture -- `input`/`output`/`cacheRead`/`cacheWrite`/`reasoning`/
 * `totalTokens` -- and its own doc comment records that the nested
 * `cost` sub-object is deliberately excluded from that sum (all-zero
 * against every real, uncosted local-model capture it was checked
 * against). Per-run dollar cost does exist elsewhere
 * (`Run.AgentEvidenceRound`'s relay-consumed-cost fields, and
 * `ProjectStats.medianAcceptedCostMicroUsd` computed from those
 * server-side) but that is run-level evidence a request-level view
 * cannot reach without a per-ticket run join this phase deliberately
 * does not do (deferred to a later server-side rollup).
 *
 * So this renders a **token total**, not a dollar figure -- the only
 * value actually available from the evidence fields -- as a cost proxy
 * inline on the row, so an operator doesn't need to leave the board to
 * see it, without inventing a $0.00 that was never computed from real
 * evidence.
 */
export function requestTokenTotal(request: RequestSummary): number | null {
  const specUsage = request.specEvidence?.usage ?? null;
  const planUsage = request.planEvidence?.usage ?? null;
  const specTotal = specUsage === null ? null : usageTotalTokens(specUsage);
  const planTotal = planUsage === null ? null : usageTotalTokens(planUsage);
  if (specTotal === null && planTotal === null) return null;
  return (specTotal ?? 0) + (planTotal ?? 0);
}

/**
 * Formats requestTokenTotal's result for display. Missing evidence
 * (both spec and plan usage absent, e.g. a request still in
 * spec_drafting) renders as an em dash -- never `0`, which would read as
 * "confirmed zero tokens" rather than "not known yet".
 */
export function formatRequestTokenTotal(tokens: number | null): string {
  if (tokens === null) return "—";
  if (tokens >= 1000) {
    const thousands = tokens / 1000;
    return `${thousands.toFixed(thousands < 10 ? 1 : 0)}k tok`;
  }
  return `${tokens} tok`;
}

/**
 * Shared token-count formatter ("823", "12.3k", "1.2M"), so every usage
 * renderer formats token counts identically.
 * Mirrors cmd/factoryd's own formatTokenCount (round_summary.go): the tenth
 * is rounded half up in integer arithmetic, because a float format rounds
 * an exact tie (1250 -> 1.25) differently in Go and in a float formatter.
 * test/fixtures/vectors/cost.json holds the cases both are tested against.
 */
export function formatTokenCount(n: number): string {
  if (n >= 1000000) {
    const tenths = Math.trunc((n + 50000) / 100000);
    return `${Math.trunc(tenths / 10)}.${tenths % 10}M`;
  }
  if (n >= 1000) {
    const tenths = Math.trunc((n + 50) / 100);
    return `${Math.trunc(tenths / 10)}.${tenths % 10}k`;
  }
  return `${n}`;
}

/**
 * Formats one ModelUsage entry as "gpt-5.6-luna · 478.3k tokens", or
 * "unknown model · 12.3k tokens" when usage.model is empty (the
 * contributing evidence recorded no model id). `complete` false prefixes
 * the token count with "≥ " -- the containing summary's tokens are a
 * lower bound, not an exact figure.
 */
export function formatModelUsageLine(usage: ModelUsage, complete = true): string {
  const model = usage.model === "" ? "unknown model" : usage.model;
  const prefix = complete ? "" : "≥ ";
  return `${model} · ${prefix}${formatTokenCount(usage.tokens)} tokens`;
}

/**
 * Formats a server-computed CostSummary's usage for a single-line
 * context (the request list row): the model's line when one model was
 * used; with several, "first-model +N more · <total> tokens", so the row
 * never shows only the first model's share of the spend. Falls back to a bare
 * token count when summary.byModel is empty but summary.tokens is
 * known (an older evidence shape with no model id recorded anywhere),
 * and "—" when nothing is known at all -- the operator explicitly does
 * not want a dollar figure here, only model id and tokens spent.
 */
export function formatUsageSummary(summary: CostSummary): string {
  const first = summary.byModel[0];
  if (first === undefined) {
    const tokens = summary.tokens;
    if (tokens === null) return "—";
    const prefix = summary.tokensComplete ? "" : "≥ ";
    return `${prefix}${formatTokenCount(tokens)} tokens`;
  }
  const firstLine = formatModelUsageLine(first, summary.tokensComplete);
  if (summary.byModel.length === 1) return firstLine;
  const prefix = summary.tokensComplete ? "" : "≥ ";
  const model = first.model === "" ? "unknown model" : first.model;
  const total = summary.tokens ?? summary.byModel.reduce((sum, m) => sum + m.tokens, 0);
  return `${model} +${summary.byModel.length - 1} more · ${prefix}${formatTokenCount(total)} tokens`;
}

/**
 * formatModelUsageDetailLine extends formatModelUsageLine with M3-C1's
 * own role + cost breakdown: "planning · gpt-5.6-luna · 150 tokens ·
 * $1.50" when usage.role/usage.costMicroUsd are populated. Falls back
 * to formatModelUsageLine's own bare "model · tokens" text when both are
 * absent (an older evidence record, or an attempt whose Kind never
 * resolves a role -- see ModelUsage.role's own doc comment), so a
 * pre-M3-C1 evidence record renders exactly as it always did. Used by
 * formatUsageLines (the triage/approve-confirm "Usage so far" lines),
 * never by the run detail screen's own bare-token rendering.
 */
export function formatModelUsageDetailLine(usage: ModelUsage, complete = true): string {
  const base = formatModelUsageLine(usage, complete);
  const rolePrefix = usage.role === "" ? "" : `${usage.role} · `;
  const costSuffix = usage.costMicroUsd > 0 ? ` · $${(usage.costMicroUsd / 1e6).toFixed(2)}` : "";
  return `${rolePrefix}${base}${costSuffix}`;
}

/**
 * Formats the "cost per accepted ticket" line from a CostSummary's own
 * acceptedTickets/costPerAcceptedTicketMicroUsd (api.CostSummary's own
 * fields) -- null when acceptedTickets is 0, so a caller renders nothing
 * rather than a misleading "$0.00 per ticket" for a request with nothing
 * accepted yet, or one computed by an older server that predates these
 * fields (both default to 0 the same way).
 */
export function formatCostPerAcceptedTicket(summary: CostSummary): string | null {
  if (summary.acceptedTickets <= 0) return null;
  const dollars = summary.costPerAcceptedTicketMicroUsd / 1e6;
  const tickets =
    summary.acceptedTickets === 1
      ? "1 accepted ticket"
      : `${summary.acceptedTickets} accepted tickets`;
  return `$${dollars.toFixed(2)} / accepted ticket (${tickets})`;
}

/**
 * Formats every model line of a CostSummary for a full-detail context
 * (triage, the approve-confirm dialog) -- one line per ModelUsage
 * entry (role + cost included via formatModelUsageDetailLine), or a
 * single fallback line when nothing recorded a model id, plus a trailing
 * "cost per accepted ticket" line (formatCostPerAcceptedTicket) when
 * this request has at least one accepted ticket.
 */
export function formatUsageLines(summary: CostSummary): string[] {
  const lines: string[] = [];
  if (summary.byModel.length === 0) {
    const tokens = summary.tokens;
    if (tokens === null) {
      lines.push("—");
    } else {
      const prefix = summary.tokensComplete ? "" : "≥ ";
      lines.push(`${prefix}${formatTokenCount(tokens)} tokens`);
    }
  } else {
    for (const usage of summary.byModel) {
      lines.push(formatModelUsageDetailLine(usage, summary.tokensComplete));
    }
  }
  const perTicket = formatCostPerAcceptedTicket(summary);
  if (perTicket !== null) lines.push(perTicket);
  return lines;
}

/**
 * Formats a project's median accepted tokens (ProjectStats.
 * medianAcceptedTokens) for display, e.g. "478.3k tokens" -- null (no
 * accepted runs yet) renders as "—". Shared by the project stats and ops
 * screens so the two never disagree about wording.
 */
export function formatMedianAcceptedTokens(tokens: number | null): string {
  if (tokens === null) return "—";
  return `${formatTokenCount(tokens)} tokens`;
}
