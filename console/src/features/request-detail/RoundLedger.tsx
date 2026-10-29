import { useState } from "react";
import { Link } from "react-router";

import { roundKindLabel } from "@/domain/challenge";
import type { RequestSummary, RequestTicket } from "@/domain/request";
import { runPath } from "@/routes/paths";
import { RetryDialog } from "@/shared/approval/RetryDialog";
import { Button } from "@/ui/Button";
import { RelativeTime } from "@/ui/RelativeTime";
import { StatusChipForToken } from "@/ui/StatusChip";

/** A ticket's branch has reached a remote: a reset to an earlier round would need a force-push. */
function pushed(ticket: RequestTicket): boolean {
  return ticket.prUrl !== "" || ticket.reviewRounds.some((round) => round.pushed);
}

/**
 * A ticket's round ledger: each build round that ended in a commit, with the
 * checks it failed and the ref that keeps it, and a "Retry from this round"
 * button that rebuilds the ticket on a new branch at that commit
 * (`factoryd retry -from <n>`). The button is offered for a quarantined or
 * halted request whose ticket was never pushed, and for the ticket being built.
 * Renders nothing for a ticket with no rounds.
 */
export function RoundLedger({
  request,
  ticket,
}: {
  readonly request: RequestSummary;
  readonly ticket: RequestTicket;
}) {
  const [round, setRound] = useState<number | null>(null);
  if (ticket.roundTags.length === 0) return null;
  const retryable =
    (request.state === "quarantined" || request.state === "halted") &&
    request.ticketIndex === ticket.index &&
    !pushed(ticket);
  const link = "text-accent underline underline-offset-2";
  return (
    <div data-testid={`round-ledger-${ticket.index}`} className="flex flex-col gap-1.5">
      <h4 className="text-fg-muted text-xs font-semibold">Rounds</h4>
      <ul className="flex flex-col gap-1.5 text-sm">
        {ticket.roundTags.map((tag) => (
          <li key={tag.n} className="flex flex-col gap-0.5">
            <div className="flex flex-wrap items-center gap-2">
              <span>{`Round ${tag.n}`}</span>
              <span className="text-fg-muted">{roundKindLabel(tag.kind)}</span>
              <StatusChipForToken token={tag.outcome} />
              <span className="text-fg-muted font-mono text-xs">{tag.sha.slice(0, 12)}</span>
              {tag.at === "" ? null : (
                <span className="text-fg-subtle text-xs">
                  <RelativeTime value={tag.at} />
                </span>
              )}
              {tag.runId === "" ? null : (
                <Link to={runPath(tag.runId)} className={link}>
                  {`Round ${tag.n} run`}
                </Link>
              )}
              <Button
                size="sm"
                disabled={!retryable}
                title={
                  retryable
                    ? undefined
                    : pushed(ticket)
                      ? "This ticket's branch was pushed: close its pull request first"
                      : "Only a quarantined or halted request can be retried"
                }
                onClick={() => {
                  setRound(tag.n);
                }}
              >
                Retry from this round
              </Button>
            </div>
            {tag.failedChecks.length === 0 ? null : (
              <p className="text-fg-muted text-xs">{`failed: ${tag.failedChecks.join(", ")}`}</p>
            )}
            <p className="text-fg-subtle font-mono text-xs break-all">{tag.ref}</p>
          </li>
        ))}
      </ul>
      <RetryDialog
        request={request}
        open={round !== null}
        round={round ?? undefined}
        onOpenChange={(open) => {
          if (!open) setRound(null);
        }}
      />
    </div>
  );
}
