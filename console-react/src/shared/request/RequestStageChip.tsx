import { statusForToken } from "@/domain/status";
import { StatusChip } from "@/ui/StatusChip";

export interface RequestStageChipProps {
  readonly state: string;
  /** A halted request that is really accepted work awaiting its pull request: a calm label, not "halted". */
  readonly awaitingPullRequest?: boolean;
  /**
   * The id of the request/run running ahead of this one, set only while it is
   * queued behind it. Shown in place of "Building": found in the operator
   * demo, a request stalled behind another build showed a chip
   * indistinguishable from one making progress.
   */
  readonly waitingOn?: string | null;
  /** The request waits on a human: a pr_review request whose PRs all wait on their reviewer takes the needs-you colour, matching its board section. */
  readonly needsYou?: boolean;
}

// Ids run to the full slug-plus-timestamp length, too long for a chip.
function shortWaitingOnId(id: string): string {
  return id.length > 12 ? `${id.substring(0, 12)}…` : id;
}

/**
 * The request's state chip, shared by the board, triage and the request
 * page. Colours come from the shared status vocabulary; pass `needsYou` from
 * `requestStageGroupOf(request) === "review"` so a waiting request is drawn
 * in the needs-you colour.
 */
export function RequestStageChip({
  state,
  awaitingPullRequest = false,
  waitingOn = null,
  needsYou = false,
}: RequestStageChipProps) {
  if (awaitingPullRequest) return <StatusChip status="done" label="accepted · awaiting PR" />;
  if (waitingOn !== null && waitingOn !== "") {
    return (
      <StatusChip
        status={statusForToken(state)}
        label={`Queued behind ${shortWaitingOnId(waitingOn)}`}
      />
    );
  }
  return <StatusChip status={needsYou ? "needsHuman" : statusForToken(state)} label={state} />;
}
