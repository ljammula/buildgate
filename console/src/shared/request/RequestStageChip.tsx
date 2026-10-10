import { headTruncate } from "@/domain/middleTruncate";
import { AWAITING_PR_LABEL, statusForToken, statusIconForToken } from "@/domain/status";
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
  if (awaitingPullRequest) return <StatusChip status="done" label={AWAITING_PR_LABEL} />;
  if (waitingOn !== null && waitingOn !== "") {
    return (
      <StatusChip
        status={statusForToken(state)}
        label={`Queued behind ${headTruncate(waitingOn, 12, "…")}`}
        icon="circle_dashed"
      />
    );
  }
  // A failure that waits on the operator (quarantined) keeps its failure
  // colour: the "Waiting on you" badge beside it says the rest.
  const own = statusForToken(state);
  // The stuck states keep their octagon; any other state drawn as waiting
  // takes the waiting icon with its colour.
  const stuck = statusIconForToken(state) === "octagon_pause";
  return (
    <StatusChip
      status={needsYou && own !== "failed" ? "needsHuman" : own}
      label={state}
      {...(stuck ? { icon: "octagon_pause" as const } : {})}
    />
  );
}
