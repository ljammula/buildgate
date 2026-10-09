import {
  type JsonObject,
  numberMap,
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
  readonly kind: string;
  readonly runId: string;
  readonly ticket: string;
  readonly at: string;
  readonly what: string;
  readonly rounds: readonly number[];
  readonly blockers: readonly string[];
  readonly changedFiles: readonly string[];
  readonly check: string;
  /** The retained output's name, relative to the run's directory; "" when none was kept. */
  readonly log: string;
  readonly excerpt: string;
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

function decodeObservation(o: JsonObject, at: string): Observation {
  return {
    kind: reqString(o, "kind", at),
    runId: reqString(o, "run_id", at),
    ticket: optString(o, "ticket", at),
    at: optString(o, "at", at),
    what: reqString(o, "what", at),
    rounds: numberList(o, "rounds", at),
    blockers: stringList(o, "blockers", at),
    changedFiles: stringList(o, "changed_files", at),
    check: optString(o, "check", at),
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
