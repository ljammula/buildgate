// Why a run stopped. The server records the reason on the run
// (halt_error), often as a chain of wrapped workflow errors with the one
// useful sentence in the middle. An operator must be able to read that
// sentence without opening a log: it was once shown nowhere, and a request
// halted on a rejected compose file said only "fix the named services"
// without naming them (found dogfooding, 2026-10-06).
import type { Run } from "@/domain/run";

export interface RunStop {
  /** The server's machine code for the stop; empty when it gave none. */
  readonly code: string;
  /** The innermost cause, without the workflow wrappers around it. */
  readonly cause: string;
  /** The whole recorded error, for a disclosure; empty when it equals `cause`. */
  readonly full: string;
  /** The factory's triage sentence; empty when none was written. */
  readonly triage: string;
}

/**
 * The innermost message of a wrapped workflow error.
 *
 * The chain reads "outer (type: A, …): middle (type: B, retryable: true):
 * CAUSE (type: wrapError, retryable: true): repeated…": everything from the
 * first "(type: wrapError" on repeats what came before, and every segment
 * before the last "): " is a wrapper. A message with neither is its own
 * cause.
 */
export function innermostCause(error: string): string {
  let text = error.trim();
  const repeated = text.indexOf(" (type: wrapError");
  if (repeated >= 0) text = text.slice(0, repeated);
  const wrapper = text.lastIndexOf("): ");
  if (wrapper >= 0 && /\(type: [^)]*$/.test(text.slice(0, wrapper))) text = text.slice(wrapper + 3);
  return text.trim();
}

/** What to show for a run that halted or was quarantined; null when it has nothing to say. */
export function runStop(
  run: Pick<Run, "state" | "haltError" | "haltReasonCode" | "triage">,
): RunStop | null {
  if (run.state !== "halted" && run.state !== "quarantined") return null;
  const cause = innermostCause(run.haltError);
  if (cause === "" && run.triage === "" && run.haltReasonCode === "") return null;
  return {
    code: run.haltReasonCode,
    cause,
    full: cause === run.haltError.trim() ? "" : run.haltError.trim(),
    triage: run.triage,
  };
}

export interface StopText {
  /** The factory's own summary of the stop, before any wrapped error. */
  readonly summary: string;
  /** The innermost cause inside the wrapped error; empty when there is no wrapped error. */
  readonly cause: string;
  /** The whole text, for a disclosure; empty when `summary` is all of it. */
  readonly full: string;
}

/**
 * Splits a request's stop reason into what to read first and what to keep a
 * click away. The factory writes "SUMMARY (workflow execution error (type:
 * …): … CAUSE (type: wrapError, …): repeated)": the summary and the innermost
 * cause are the two sentences an operator needs; the chain between and after
 * them is receipts. A reason with no wrapped error is all summary.
 */
export function stopText(reason: string): StopText {
  const text = reason.trim();
  const wrapped = text.indexOf(" (workflow execution error");
  if (wrapped < 0) return { summary: text, cause: "", full: "" };
  const cause = innermostCause(text.slice(wrapped + 2));
  return { summary: text.slice(0, wrapped).trim(), cause, full: text };
}
