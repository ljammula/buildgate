import { statusForToken } from "@/domain/status";
import { StatusChip } from "@/ui/StatusChip";

export interface RequestStateChipProps {
  readonly state: string;
  /** A halted request that is really accepted work awaiting its pull request: a calm label, not "halted". */
  readonly awaitingPullRequest: boolean;
  /** The request/run running ahead of this one, set only while queued behind it. */
  readonly waitingOn: string | null;
  /** Waiting on a human: takes the needs-you colour. */
  readonly needsYou: boolean;
}

// Ids run to the full slug-plus-timestamp length, too long for a chip.
function shortId(id: string): string {
  return id.length > 12 ? `${id.substring(0, 12)}…` : id;
}

/** The request's state chip, coloured from the shared status vocabulary. */
export function RequestStateChip({
  state,
  awaitingPullRequest,
  waitingOn,
  needsYou,
}: RequestStateChipProps) {
  if (awaitingPullRequest) return <StatusChip status="done" label="accepted · awaiting PR" />;
  if (waitingOn !== null && waitingOn !== "") {
    return (
      <StatusChip status={statusForToken(state)} label={`Queued behind ${shortId(waitingOn)}`} />
    );
  }
  return <StatusChip status={needsYou ? "needsHuman" : statusForToken(state)} label={state} />;
}
