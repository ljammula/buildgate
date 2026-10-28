import { Link } from "react-router";

import type { RequestTicket } from "@/domain/request";
import { runPath } from "@/routes/paths";
import { EscapedText } from "@/shared/oracle/EscapedText";
import { RelativeTime } from "@/ui/RelativeTime";
import { StatusChipForToken } from "@/ui/StatusChip";

function effect(outcome: string, pushed: boolean): string {
  if (pushed) return "pushed: the pull request now has its commit";
  return outcome === "accepted"
    ? "accepted, not pushed: the run it followed stands"
    : "nothing pushed: the run it followed stands";
}

/**
 * A ticket's advice rounds: the one under way, then each that ended, newest
 * first. An advice round is one more build of an accepted ticket, given the
 * high-severity findings of its code review; only an accepted, pushed round
 * changes the pull request, and none ever refuses the ticket. Renders
 * nothing for a ticket that had none.
 */
export function AdviceRounds({ ticket }: { readonly ticket: RequestTicket }) {
  const ended = [...ticket.adviceRounds].reverse();
  const active = ticket.advice;
  if (ended.length === 0 && active === null) return null;
  const link = "text-accent underline underline-offset-2";
  return (
    <div data-testid={`ticket-advice-rounds-${ticket.index}`} className="flex flex-col gap-1.5">
      <h4 className="text-fg-muted text-xs font-semibold">Advice rounds</h4>
      <ul className="flex flex-col gap-1.5 text-sm">
        {active === null ? null : (
          <li className="flex flex-wrap items-center gap-2">
            <span>Advice round</span>
            <StatusChipForToken token="slice_running" />
            <span className="text-fg-muted">
              <EscapedText text={`checking the review's concerns about run ${active.of}`} />
            </span>
            {active.runId === "" ? null : (
              <Link to={runPath(active.runId)} className={link}>
                Follow the run
              </Link>
            )}
          </li>
        )}
        {ended.map((round) => (
          <li key={round.index} className="flex flex-col gap-0.5">
            <div className="flex flex-wrap items-center gap-2">
              <StatusChipForToken token={round.outcome} />
              <span className="text-fg-muted">{effect(round.outcome, round.pushed)}</span>
              {round.at === "" ? null : (
                <span className="text-fg-subtle text-xs">
                  <RelativeTime value={round.at} />
                </span>
              )}
              {round.runId === "" ? null : (
                <Link to={runPath(round.runId)} className={link}>
                  Advice round run
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
