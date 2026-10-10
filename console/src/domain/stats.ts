// GET /stats: the numbers `factoryd stats` prints, for every project at once.
import { type JsonObject, objectList, reqObject } from "@/domain/decode";
import { formatTokenCount } from "@/domain/cost";
import {
  type ProjectTrend,
  type TrendMetrics,
  decodeProjectTrend,
  medianText,
} from "@/domain/trend";

/** One report over every project, and one per project (internal/stats.Report, as the trend route returns it). */
export interface FactoryStats {
  /** Its `project` is "". */
  readonly overall: ProjectTrend;
  readonly projects: readonly ProjectTrend[];
}

export function decodeFactoryStats(o: JsonObject, at: string): FactoryStats {
  return {
    overall: reqObject(o, "overall", at, decodeProjectTrend),
    projects: objectList(o, "projects", at, decodeProjectTrend),
  };
}

/** One row of the Numbers table, every cell already text. "-" is "nothing to show". */
export interface NumbersRow {
  /** "Overall", or the project. */
  readonly label: string;
  readonly tickets: string;
  readonly oneShot: string;
  readonly accepted: string;
  readonly medianRounds: string;
  /** The check that quarantined the most runs: "canonical_verify (2)". */
  readonly topQuarantine: string;
  /** "478.3k tokens · $1.50"; the dollar part only when a cost was recorded. */
  readonly spend: string;
  readonly costPerAccepted: string;
}

function dollars(microUsd: number): string {
  return `$${(microUsd / 1e6).toFixed(2)}`;
}

/**
 * "1/3 (33%)" from the server's own rate; "-" when the rate is null (no
 * ticket to take it over), never 0%.
 */
export function rateText(part: number, whole: number, rate: number | null): string {
  return rate === null ? "-" : `${part}/${whole} (${Math.round(rate * 100)}%)`;
}

/** "478.3k tokens · $1.50", "12.3k tokens" with no recorded cost, "-" with no spend at all. */
export function spendText(metrics: TrendMetrics): string {
  if (metrics.spendTokens === 0 && metrics.spendCostMicroUsd === 0) return "-";
  const tokens = `${formatTokenCount(metrics.spendTokens)} tokens`;
  return metrics.spendCostMicroUsd > 0
    ? `${tokens} · ${dollars(metrics.spendCostMicroUsd)}`
    : tokens;
}

/** "$0.75"; "-" until a ticket is accepted with a recorded cost. */
export function costPerAcceptedText(metrics: TrendMetrics): string {
  return metrics.accepted > 0 && metrics.perAcceptedTicketMicroUsd > 0
    ? dollars(metrics.perAcceptedTicketMicroUsd)
    : "-";
}

function numbersRow(label: string, metrics: TrendMetrics): NumbersRow {
  // The server sorts quarantined_by largest first.
  const top = metrics.quarantinedBy[0];
  return {
    label,
    tickets: String(metrics.tickets),
    oneShot: rateText(metrics.oneShot, metrics.tickets, metrics.oneShotRate),
    accepted: rateText(metrics.accepted, metrics.tickets, metrics.acceptedRate),
    medianRounds: medianText(metrics.roundsToGreen),
    topQuarantine: top === undefined ? "-" : `${top.name} (${top.runs})`,
    spend: spendText(metrics),
    costPerAccepted: costPerAcceptedText(metrics),
  };
}

/** "Overall" first, then one row per project in the server's order. */
export function numbersRows(stats: FactoryStats): NumbersRow[] {
  return [
    numbersRow("Overall", stats.overall.overall),
    ...stats.projects.map((report) => numbersRow(report.project, report.overall)),
  ];
}

/** No ticket has a finished run anywhere: there is no number to show yet. */
export function statsEmpty(stats: FactoryStats): boolean {
  return stats.overall.overall.tickets === 0 && stats.projects.length === 0;
}
