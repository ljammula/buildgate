import { type JsonObject, objectList, optNumber, optString, reqString } from "@/domain/decode";

/**
 * One failed check of a stopped run: internal/handoff.Check. `finding` is the
 * factory's sentence and may quote a log line; it is shown as text.
 */
export interface HandoffCheck {
  readonly check: string;
  /** corrective, corrective_if_oracle_in_loop, never or operator. */
  readonly bin: string;
  readonly exitCode: number | null;
  readonly finding: string;
}

/**
 * internal/handoff.Document, GET /runs/{id}/handoff: what a stopped run left
 * for a later attempt. Only the parts the run page does not already show
 * elsewhere are decoded: its rounds are on the Timeline.
 */
export interface Handoff {
  readonly runId: string;
  readonly state: string;
  /** The most restrictive bin of the failed checks; "operator" for a halt. */
  readonly next: string;
  readonly checks: readonly HandoffCheck[];
}

function decodeCheck(o: JsonObject, at: string): HandoffCheck {
  return {
    check: reqString(o, "check", at),
    bin: reqString(o, "bin", at),
    exitCode: optNumber(o, "exit_code", at),
    finding: optString(o, "finding", at),
  };
}

export function decodeHandoff(o: JsonObject, at: string): Handoff {
  return {
    runId: reqString(o, "run_id", at),
    state: reqString(o, "state", at),
    next: reqString(o, "next", at),
    checks: objectList(o, "checks", at, decodeCheck),
  };
}

/**
 * What a bin means, said of one check. A bin this console does not know is
 * shown under its own name: the server may add one.
 */
const binLabels: Readonly<Record<string, string>> = {
  corrective: "Of a kind a build can fix when told what failed",
  corrective_if_oracle_in_loop: "May be told to a build only where it may see the reference oracle",
  never: "Of a kind a build is never told about",
  operator: "Needs a change outside the build",
};

export function handoffBinLabel(bin: string): string {
  return binLabels[bin] ?? bin;
}

/**
 * What the failures allow, said of the run as a whole. It describes how the
 * failure is sorted; it does not say a build has been or will be started.
 */
const nextSentences: Readonly<Record<string, string>> = {
  corrective: "Every failed check is of a kind a build can fix when it is told what failed.",
  corrective_if_oracle_in_loop:
    "The reference oracle failed. Telling a build what it asserted would expose it, so this may be told to a build only on a run that already lets the build see the oracle.",
  never:
    "At least one failed check is of a kind a build is never told about, so this is not handed back to a build.",
  operator: "The build cannot fix this. Something outside it has to change before another attempt.",
};

export function handoffNextSentence(next: string): string {
  return nextSentences[next] ?? `Sorted as: ${next}.`;
}
