// Where a request is on the pipeline, read from its own record: its state,
// its history and, while it waits in resume_review, the lost step. The board
// places a card by it and the request page marks its steps by it, so the two
// always agree. Structural facts only: never the text of an error.
import type { RequestSummary, RequestTransition } from "@/domain/request";

/** The canonical, linear pipeline of request states, first to last. */
export const PIPELINE_STATES = [
  "submitted",
  "spec_drafting",
  "spec_review",
  "oracle_drafting",
  "oracle_review",
  "planning",
  "plan_review",
  "building",
  "pr_review",
  "done",
] as const;

/** The states that stop a request where it was, until the operator acts: not steps of the pipeline. */
export const STUCK_STATES: ReadonlySet<string> = new Set([
  "halted",
  "quarantined",
  "resume_review",
]);

/** The ends a request can come to: merged, a pull request closed without merging, withdrawn. */
export const FINISHED_STATES: ReadonlySet<string> = new Set(["done", "closed", "cancelled"]);

/**
 * The step a stopped request stopped at, and the move that took it out of
 * it. The last entry's `from` is not enough: a request cancelled out of
 * quarantine moved `quarantined -> cancelled`, and `quarantined` is not a
 * pipeline step. The most recent move out of a pipeline step is where it
 * stopped. Null with no such move in the history.
 */
export function stoppedAt(
  request: RequestSummary,
  steps: readonly string[] = PIPELINE_STATES,
): { readonly step: string; readonly entry: RequestTransition } | null {
  for (let i = request.history.length - 1; i >= 0; i--) {
    const entry = request.history[i];
    if (entry !== undefined && steps.includes(entry.from)) return { step: entry.from, entry };
  }
  return null;
}

/**
 * The pipeline state a request is at: its own state when that is a step of
 * the pipeline; for resume_review the lost step it records; for any other
 * state the step it stopped at (`stoppedAt`). A record that says none of
 * this keeps its own state, so the caller still sees what it is.
 */
export function pipelineStateOf(request: RequestSummary): string {
  if ((PIPELINE_STATES as readonly string[]).includes(request.state)) return request.state;
  if (request.state === "resume_review") {
    const lost = request.resume?.fromState ?? "";
    if ((PIPELINE_STATES as readonly string[]).includes(lost)) return lost;
  }
  return stoppedAt(request)?.step ?? request.state;
}
