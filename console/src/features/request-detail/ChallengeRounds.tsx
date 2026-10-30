import { Link } from "react-router";

import type { RequestTicket } from "@/domain/request";
import { runPath } from "@/routes/paths";
import { EscapedText } from "@/shared/text/EscapedText";
import { RelativeTime } from "@/ui/RelativeTime";
import { StatusChipForToken } from "@/ui/StatusChip";

/**
 * A ticket's challenge round: one more build of the ticket, given the held
 * test files the challenger wrote that its code failed (SC-022). Renders
 * nothing for a ticket that had none.
 */
export function ChallengeRounds({ ticket }: { readonly ticket: RequestTicket }) {
  const ended = [...ticket.challengeRounds].reverse();
  if (ended.length === 0) return null;
  const link = "text-accent underline underline-offset-2";
  return (
    <div data-testid={`ticket-challenge-rounds-${ticket.index}`} className="flex flex-col gap-1.5">
      <h4 className="text-fg-muted text-xs font-semibold">Challenge rounds</h4>
      <ul className="flex flex-col gap-1.5 text-sm">
        {ended.map((round) => (
          <li key={round.index} className="flex flex-col gap-0.5">
            <div className="flex flex-wrap items-center gap-2">
              <span>Challenge round</span>
              <StatusChipForToken token={round.outcome} />
              {round.at === "" ? null : (
                <span className="text-fg-subtle text-xs">
                  <RelativeTime value={round.at} />
                </span>
              )}
              {round.runId === "" ? null : (
                <Link to={runPath(round.runId)} className={link}>
                  Challenge round run
                </Link>
              )}
            </div>
            {round.error === "" ? null : (
              <p className="font-mono text-xs break-words whitespace-pre-wrap">
                <EscapedText text={round.error} />
              </p>
            )}
          </li>
        ))}
      </ul>
    </div>
  );
}
