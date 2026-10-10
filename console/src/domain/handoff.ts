import {
  type JsonObject,
  objectList,
  optBoolean,
  optObject,
  optNumber,
  optString,
  reqString,
  stringList,
} from "@/domain/decode";

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
  /** The failing lines of a command gate's output; empty for other checks. */
  readonly output: readonly string[];
  /**
   * True for a check on the attempt's diff that failed only because the
   * attempt committed nothing: it says nothing about the work and is not
   * counted in `next`.
   */
  readonly notJudged: boolean;
}

/**
 * internal/handoff.Notes: what the build agent itself said for whoever
 * attempts the ticket next, as one-line items under five fixed headings. It
 * is the agent's view, not the factory's record, and agent-written: shown as
 * text, labelled unverified, and read from the handoff route only.
 */
export interface HandoffNotes {
  readonly did: readonly string[];
  readonly triedAndFailed: readonly string[];
  readonly hypothesis: readonly string[];
  readonly leftToDo: readonly string[];
  readonly repository: readonly string[];
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
  /** null when the agent left none. */
  readonly agentNotes: HandoffNotes | null;
}

function decodeNotes(o: JsonObject, at: string): HandoffNotes {
  return {
    did: stringList(o, "did", at),
    triedAndFailed: stringList(o, "tried_and_failed", at),
    hypothesis: stringList(o, "hypothesis", at),
    leftToDo: stringList(o, "left_to_do", at),
    repository: stringList(o, "repository", at),
  };
}

function decodeCheck(o: JsonObject, at: string): HandoffCheck {
  return {
    check: reqString(o, "check", at),
    bin: reqString(o, "bin", at),
    exitCode: optNumber(o, "exit_code", at),
    finding: optString(o, "finding", at),
    output: stringList(o, "output", at),
    notJudged: optBoolean(o, "not_judged", at),
  };
}

export function decodeHandoff(o: JsonObject, at: string): Handoff {
  return {
    runId: reqString(o, "run_id", at),
    state: reqString(o, "state", at),
    next: reqString(o, "next", at),
    checks: objectList(o, "checks", at, decodeCheck),
    agentNotes: optObject(o, "agent_notes", at, decodeNotes),
  };
}

export interface HandoffNoteSection {
  readonly label: string;
  readonly items: readonly string[];
}

/**
 * The notes as the record given to a later build lists them: the same five
 * labels in the same order, a heading with no item left out.
 */
export function handoffNoteSections(notes: HandoffNotes | null): readonly HandoffNoteSection[] {
  if (notes === null) return [];
  return [
    { label: "What it did", items: notes.did },
    { label: "What it tried that did not work", items: notes.triedAndFailed },
    { label: "Its hypothesis", items: notes.hypothesis },
    { label: "What it said was left to do", items: notes.leftToDo },
    { label: "What it said about this repository", items: notes.repository },
  ].filter((section) => section.items.length > 0);
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

/** Said of a check that was not judged, in place of its bin. */
export const HANDOFF_NOT_JUDGED =
  "Not judged: the attempt committed nothing, so there was no diff to check";

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
