import {
  type JsonObject,
  numberMap,
  numberOr,
  objectList,
  optBoolean,
  optString,
  reqNumber,
  reqString,
  stringList,
} from "@/domain/decode";

/**
 * One fact from one finished run: internal/observation.Observation. The
 * sentence in `what` is the factory's; the values it and the other fields
 * carry (blocker names, file names, the output excerpt) are the build
 * agent's own report, so they are shown as text.
 */
export interface Observation {
  /** 16 hex characters, stable across reads. */
  readonly id: string;
  readonly kind: string;
  /** The record it was derived from: "run" or "request". */
  readonly source: string;
  /** The run it is about (the quarantined one for check_fixed); "" for an operator edit. */
  readonly runId: string;
  readonly ticket: string;
  readonly at: string;
  readonly what: string;
  readonly rounds: readonly number[];
  readonly blockers: readonly string[];
  readonly changedFiles: readonly string[];
  readonly check: string;
  /** The failing round's failure signature; "" when it is not about a failed round. */
  readonly signature: string;
  /** check_fixed: the run that was accepted after the quarantined one. */
  readonly acceptedRunId: string;
  /** check_fixed: each failed check with the factory's own sentence about it ("" when none). */
  readonly checks: readonly ObservationCheck[];
  /** Request-derived kinds: the request, and the ticket (0 when none). */
  readonly requestId: string;
  readonly ticketIndex: number;
  /** review_comment_accepted: review thread ids, never a comment. */
  readonly threadIds: readonly string[];
  /** operator_edit: the review gate. */
  readonly stage: string;
  /** operator_edit: file and section names the action touched, never text. */
  readonly anchors: readonly string[];
  /** The retained output's name, relative to the run's directory; "" when none was kept. */
  readonly log: string;
  readonly excerpt: string;
}

/** internal/observation.CheckNote. */
export interface ObservationCheck {
  readonly check: string;
  readonly sentence: string;
}

/** internal/observation.Report: GET /projects/{project}/observations. */
export interface ObservationReport {
  readonly project: string;
  /** Finished runs read. */
  readonly runs: number;
  /** Of them, accepted after one passing round with no override or rescue. */
  readonly acceptedFirstRound: number;
  /** Observations of each kind, before the list was cut. */
  readonly counts: Readonly<Record<string, number>>;
  /** Newest run first. */
  readonly observations: readonly Observation[];
  /** True when the list is shorter than the counts say. */
  readonly truncated: boolean;
}

function numberList(o: JsonObject, key: string, at: string): number[] {
  const value = o[key];
  if (value === undefined || value === null) return [];
  if (!Array.isArray(value) || value.some((n) => typeof n !== "number")) {
    throw new Error(`${at}.${key}: expected a list of numbers`);
  }
  return value as number[];
}

function decodeCheck(o: JsonObject, at: string): ObservationCheck {
  return { check: reqString(o, "check", at), sentence: optString(o, "sentence", at) };
}

function decodeObservation(o: JsonObject, at: string): Observation {
  return {
    id: optString(o, "id", at),
    kind: reqString(o, "kind", at),
    source: optString(o, "source", at),
    runId: reqString(o, "run_id", at),
    ticket: optString(o, "ticket", at),
    at: optString(o, "at", at),
    what: reqString(o, "what", at),
    rounds: numberList(o, "rounds", at),
    blockers: stringList(o, "blockers", at),
    changedFiles: stringList(o, "changed_files", at),
    check: optString(o, "check", at),
    signature: optString(o, "signature", at),
    acceptedRunId: optString(o, "accepted_run_id", at),
    checks: objectList(o, "checks", at, decodeCheck),
    requestId: optString(o, "request_id", at),
    ticketIndex: numberOr(o, "ticket_index", at, 0),
    threadIds: stringList(o, "thread_ids", at),
    stage: optString(o, "stage", at),
    anchors: stringList(o, "anchors", at),
    log: optString(o, "log", at),
    excerpt: optString(o, "excerpt", at),
  };
}

export function decodeObservationReport(o: JsonObject, at: string): ObservationReport {
  return {
    project: reqString(o, "project", at),
    runs: reqNumber(o, "runs", at),
    acceptedFirstRound: reqNumber(o, "accepted_first_round", at),
    counts: numberMap(o, "counts", at),
    observations: objectList(o, "observations", at, decodeObservation),
    truncated: optBoolean(o, "truncated", at),
  };
}

/**
 * What each kind is called on the page, in the order the server lists them.
 * A kind this console does not know is shown under its own name.
 */
const kindLabels: Readonly<Record<string, string>> = {
  fixed_after_failure: "Fixed after a failed round",
  repeated_failure: "Same failure twice running",
  round_changed_nothing: "A round changed nothing",
  check_failed: "A check failed and the run was quarantined",
  run_halted: "The run halted",
  check_fixed: "A quarantined check was fixed by a later run",
  review_comment_accepted: "A review round was accepted and pushed",
  operator_edit: "The operator edited or sent back a draft",
};

export function observationKindLabel(kind: string): string {
  return kindLabels[kind] ?? kind;
}

/** The kinds that have at least one observation, known kinds first, in page order. */
export function observationCounts(report: ObservationReport): readonly [string, number][] {
  const known = Object.keys(kindLabels);
  const rank = (kind: string) => (known.includes(kind) ? known.indexOf(kind) : known.length);
  return Object.entries(report.counts)
    .filter(([, count]) => count > 0)
    .sort(([a], [b]) => rank(a) - rank(b) || a.localeCompare(b));
}
