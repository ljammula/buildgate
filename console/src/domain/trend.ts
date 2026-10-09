import {
  type JsonObject,
  numberOr,
  objectList,
  optNumber,
  optString,
  reqNumber,
  reqObject,
  reqString,
} from "@/domain/decode";

/** A check or a halt reason and how many runs it stopped: internal/stats.Count. */
export interface TrendCount {
  readonly name: string;
  readonly runs: number;
}

/** The median and 90th percentile of a count over several tickets: internal/stats.Spread. */
export interface TrendSpread {
  /** How many tickets it was taken over; 0 means there is nothing to show. */
  readonly series: number;
  readonly median: number;
  readonly p90: number;
}

/** The numbers over a set of tickets: internal/stats.Metrics. */
export interface TrendMetrics {
  readonly tickets: number;
  readonly oneShot: number;
  /** null when there are no tickets. */
  readonly oneShotRate: number | null;
  readonly accepted: number;
  readonly acceptedRate: number | null;
  readonly roundsToGreen: TrendSpread;
  readonly failedRoundPairs: number;
  readonly comparablePairs: number;
  readonly sameFailurePairs: number;
  readonly noChangePairs: number;
  readonly quarantinedBy: readonly TrendCount[];
  readonly haltedBy: readonly TrendCount[];
  readonly correctiveRan: number;
  readonly correctiveAccepted: number;
  readonly spendTokens: number;
  readonly spendCostMicroUsd: number;
  readonly perAcceptedTicketMicroUsd: number;
}

/** Metrics over the tickets whose first run began in [start, end). */
export interface TrendBucket {
  readonly start: string;
  readonly end: string;
  readonly metrics: TrendMetrics;
}

/** internal/stats.Report: GET /projects/{project}/trend. */
export interface ProjectTrend {
  readonly project: string;
  readonly since: string;
  readonly until: string;
  readonly bucketDays: number;
  readonly runs: number;
  readonly unfinished: number;
  readonly excludedRuns: number;
  readonly overall: TrendMetrics;
  readonly buckets: readonly TrendBucket[];
}

function decodeCount(o: JsonObject, at: string): TrendCount {
  return { name: reqString(o, "name", at), runs: numberOr(o, "runs", at, 0) };
}

function decodeSpread(o: JsonObject, at: string): TrendSpread {
  return {
    series: numberOr(o, "series", at, 0),
    median: numberOr(o, "median", at, 0),
    p90: numberOr(o, "p90", at, 0),
  };
}

function nested(o: JsonObject, key: string, at: string): JsonObject {
  return reqObject(o, key, at, (value) => value);
}

function decodeMetrics(o: JsonObject, at: string): TrendMetrics {
  const corrective = nested(o, "corrective_builds", at);
  const spend = nested(o, "spend", at);
  return {
    tickets: reqNumber(o, "tickets", at),
    oneShot: numberOr(o, "one_shot", at, 0),
    oneShotRate: optNumber(o, "one_shot_rate", at),
    accepted: numberOr(o, "accepted", at, 0),
    acceptedRate: optNumber(o, "accepted_rate", at),
    roundsToGreen: reqObject(o, "rounds_to_green", at, decodeSpread),
    failedRoundPairs: numberOr(o, "failed_round_pairs", at, 0),
    comparablePairs: numberOr(o, "comparable_pairs", at, 0),
    sameFailurePairs: numberOr(o, "same_failure_pairs", at, 0),
    noChangePairs: numberOr(o, "no_change_pairs", at, 0),
    quarantinedBy: objectList(o, "quarantined_by", at, decodeCount),
    haltedBy: objectList(o, "halted_by", at, decodeCount),
    correctiveRan: numberOr(corrective, "ran", `${at}.corrective_builds`, 0),
    correctiveAccepted: numberOr(corrective, "accepted", `${at}.corrective_builds`, 0),
    spendTokens: numberOr(spend, "tokens", `${at}.spend`, 0),
    spendCostMicroUsd: numberOr(spend, "cost_micro_usd", `${at}.spend`, 0),
    perAcceptedTicketMicroUsd: numberOr(spend, "per_accepted_ticket_micro_usd", `${at}.spend`, 0),
  };
}

function decodeBucket(o: JsonObject, at: string): TrendBucket {
  return {
    start: reqString(o, "start", at),
    end: reqString(o, "end", at),
    metrics: reqObject(o, "metrics", at, decodeMetrics),
  };
}

export function decodeProjectTrend(o: JsonObject, at: string): ProjectTrend {
  return {
    project: reqString(o, "project", at),
    since: optString(o, "since", at),
    until: optString(o, "until", at),
    bucketDays: reqNumber(o, "bucket_days", at),
    runs: numberOr(o, "runs", at, 0),
    unfinished: numberOr(o, "unfinished", at, 0),
    excludedRuns: numberOr(o, "excluded_runs", at, 0),
    overall: reqObject(o, "overall", at, decodeMetrics),
    buckets: objectList(o, "buckets", at, decodeBucket),
  };
}

/** "3/8 (38%)", "-" when there is no denominator. The same text `factoryd stats` prints. */
export function shareText(part: number, whole: number): string {
  if (whole === 0) return "-";
  return `${part}/${whole} (${Math.round((100 * part) / whole)}%)`;
}

/** The median of a spread, "-" when it was taken over no ticket. */
export function medianText(spread: TrendSpread): string {
  return spread.series === 0 ? "-" : String(spread.median);
}

/** The date of a bucket's start: "2026-09-27". */
export function bucketLabel(bucket: TrendBucket): string {
  return bucket.start.slice(0, 10);
}
