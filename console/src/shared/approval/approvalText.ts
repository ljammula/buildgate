/**
 * The state Approve moves `state` to -- internal/request's transition table.
 * A fallback only: `RequestSummary.approveNextState` (the server's own
 * answer) always wins when non-empty.
 */
export function nextStateAfterApprove(state: string): string | null {
  switch (state) {
    case "spec_review":
      return "planning";
    case "plan_review":
      return "building";
    default:
      return null;
  }
}
