import {
  type JsonObject,
  objectList,
  optBoolean,
  optString,
  reqString,
  stringList,
} from "@/domain/decode";

/** One failing test of a red challenge file (Go's run.ChallengeFailure). */
export interface ChallengeFailure {
  readonly test: string;
  readonly assertion: string;
}

/**
 * One test file of a challenge (Go's run.ChallengeFile). `verdict` is
 * "green", "red", "broken" or "advice" (red once, then green); "" for a file
 * that was not run. The file's content is never sent.
 */
export interface ChallengeFile {
  readonly path: string;
  /** The header's label, "AC-<n>" or "answer-<n>". */
  readonly criterion: string;
  readonly verdict: string;
  /** The shape rule that kept the file out; "" for a held file. */
  readonly refused: string;
  /** Why a held file was not run; "" otherwise. */
  readonly notRun: string;
  readonly failures: readonly ChallengeFailure[];
  /**
   * True for a file no runner covers, which ran together with the other such
   * files under the repository's verify command: its verdict and failures
   * are that one run's, never a verdict on this file alone.
   */
  readonly group: boolean;
}

/**
 * A run's challenge (Go's run.ChallengeRecord, SC-022). `verdict` is "green",
 * "red" or "no_verdict"; `cause` names why for the last.
 */
export interface Challenge {
  readonly verdict: string;
  readonly cause: string;
  /** True for a run that ran the held files of an earlier challenge. */
  readonly rerun: boolean;
  /** The commit that holds the green files; "" when none. */
  readonly committedSha: string;
  readonly files: readonly ChallengeFile[];
  readonly notes: readonly string[];
}

function decodeChallengeFailure(o: JsonObject, at: string): ChallengeFailure {
  return { test: reqString(o, "test", at), assertion: optString(o, "assertion", at) };
}

function decodeChallengeFile(o: JsonObject, at: string): ChallengeFile {
  return {
    path: reqString(o, "path", at),
    criterion: optString(o, "criterion", at),
    verdict: optString(o, "verdict", at),
    refused: optString(o, "refused", at),
    notRun: optString(o, "not_run", at),
    failures: objectList(o, "failures", at, decodeChallengeFailure),
    group: optBoolean(o, "group", at),
  };
}

export function decodeChallenge(o: JsonObject, at: string): Challenge {
  return {
    verdict: reqString(o, "verdict", at),
    cause: optString(o, "cause", at),
    rerun: optBoolean(o, "rerun", at),
    committedSha: optString(o, "committed_sha", at),
    files: objectList(o, "files", at, decodeChallengeFile),
    notes: stringList(o, "notes", at),
  };
}

const VERDICT_LABELS: Readonly<Record<string, string>> = {
  green: "Green",
  red: "Red",
  no_verdict: "No verdict",
  broken: "Broken",
  advice: "Advice (red once, then green)",
};

/** A readable word for a challenge or file verdict; an unknown one is shown as sent. */
export function challengeVerdictLabel(verdict: string): string {
  return VERDICT_LABELS[verdict] ?? (verdict === "" ? "Not run" : verdict);
}

const ROUND_KIND_LABELS: Readonly<Record<string, string>> = {
  advice: "Advice round",
  challenge: "Challenge round",
};

/** The label of a round kind in a ticket's round list; an unknown kind is shown as sent. */
export function roundKindLabel(kind: string): string {
  return ROUND_KIND_LABELS[kind] ?? kind;
}
