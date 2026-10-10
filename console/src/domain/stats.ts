// GET /stats: the numbers `factoryd stats` prints, for every project at once.
import { type JsonObject, objectList, reqObject } from "@/domain/decode";
import { formatTokenCount } from "@/domain/cost";
import type { BoardWindowDays } from "@/domain/boardFilters";
import { NO_VALUE } from "@/domain/noValue";
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

/** One row of the Numbers table, every cell already text. `NO_VALUE` is "nothing to show". */
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
 * "1/3 (33%)" from the server's own rate; NO_VALUE when the rate is null (no
 * ticket to take it over), never 0%.
 */
export function rateText(part: number, whole: number, rate: number | null): string {
  return rate === null ? NO_VALUE : `${part}/${whole} (${Math.round(rate * 100)}%)`;
}

/** "478.3k tokens · $1.50", "12.3k tokens" with no recorded cost, "–" with no spend at all. */
export function spendText(metrics: TrendMetrics): string {
  if (metrics.spendTokens === 0 && metrics.spendCostMicroUsd === 0) return NO_VALUE;
  const tokens = `${formatTokenCount(metrics.spendTokens)} tokens`;
  return metrics.spendCostMicroUsd > 0
    ? `${tokens} · ${dollars(metrics.spendCostMicroUsd)}`
    : tokens;
}

/** "$0.75"; "–" until a ticket is accepted with a recorded cost. */
export function costPerAcceptedText(metrics: TrendMetrics): string {
  return metrics.accepted > 0 && metrics.perAcceptedTicketMicroUsd > 0
    ? dollars(metrics.perAcceptedTicketMicroUsd)
    : NO_VALUE;
}

function numbersRow(label: string, metrics: TrendMetrics): NumbersRow {
  // The server sorts quarantined_by largest first.
  const top = metrics.quarantinedBy[0];
  return {
    label,
    tickets: String(metrics.tickets),
    oneShot: rateText(metrics.oneShot, metrics.tickets, metrics.oneShotRate),
    accepted: rateText(metrics.accepted, metrics.tickets, metrics.acceptedRate),
    medianRounds: metrics.roundsToGreen.series === 0 ? NO_VALUE : medianText(metrics.roundsToGreen),
    topQuarantine: top === undefined ? NO_VALUE : `${top.name} (${top.runs})`,
    spend: spendText(metrics),
    costPerAccepted: costPerAcceptedText(metrics),
  };
}

/**
 * "Overall" first, then one row per project in the server's order. With
 * projects chosen in the toolbar (`projects` not empty) only their rows: the
 * overall row is every project's, which the filter has put out of view.
 */
export function numbersRows(
  stats: FactoryStats,
  projects: ReadonlySet<string> = new Set(),
): NumbersRow[] {
  const perProject = stats.projects
    .filter((report) => projects.size === 0 || projects.has(report.project))
    .map((report) => numbersRow(report.project, report.overall));
  return projects.size === 0
    ? [numbersRow("Overall", stats.overall.overall), ...perProject]
    : perProject;
}

/** No ticket has a finished run anywhere: there is no number to show yet. */
export function statsEmpty(stats: FactoryStats): boolean {
  return stats.overall.overall.tickets === 0 && stats.projects.length === 0;
}

/** No row of the table has a ticket in the window: a table of dashes would say nothing. */
export function numbersHaveNoTickets(rows: readonly NumbersRow[]): boolean {
  // A row with no ticket can still carry spend or a quarantining check
  // (a run that never produced a ticket result): that is something to show.
  return rows.every(
    (row) => row.tickets === "0" && row.spend === NO_VALUE && row.topQuarantine === NO_VALUE,
  );
}

/** The one line that stands in for a table with no ticket in the window. */
export function noTicketsText(days: BoardWindowDays): string {
  return days === "all"
    ? "No finished tickets in all time"
    : `No finished tickets in the last ${days} days`;
}
