import type { RequestSummary } from "@/domain/request";

/**
 * The state Approve moves `state` to -- internal/request's transition table.
 * A fallback only: `RequestSummary.approveNextState` (the server's own
 * answer) always wins when non-empty, because this table has drifted before
 * (it said spec_review -> planning for a -draft-oracles request that
 * actually goes to oracle_drafting).
 */
export function nextStateAfterApprove(state: string): string | null {
  switch (state) {
    case "spec_review":
    case "oracle_review":
      return "planning";
    case "plan_review":
      return "building";
    default:
      return null;
  }
}

/**
 * The warning to show before approving oracle_review when the approval will
 * pin no oracle files after a draft that was not a deliberate none_eligible
 * (or no-drafter) outcome; null when there is nothing to warn about.
 * `expectedSha256` is the map the approval will send (the displayed oracle
 * files' hashes); null for any other kind of approval.
 */
export function oracleSkipWarningFor(
  request: RequestSummary,
  expectedSha256: Readonly<Record<string, string>> | null,
): string | null {
  if (request.state !== "oracle_review") return null;
  if (expectedSha256 === null || Object.keys(expectedSha256).length > 0) return null;
  const status = request.oracleDraftStatus;
  if (status === "" || status === "none_eligible" || status === "not_implemented") return null;
  const detail = request.oracleDraftDetail;
  return (
    `Approving skips the oracle stage: the draft ended "${status}" and no ` +
    `oracle files exist, so this request will be built with no acceptance ` +
    `oracle.${detail === "" ? "" : ` (${detail})`}`
  );
}
