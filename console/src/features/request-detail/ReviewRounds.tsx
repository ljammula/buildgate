import { Link } from "react-router";

import type { RequestTicket } from "@/domain/request";
import { runPath } from "@/routes/paths";
import { EscapedText } from "@/shared/oracle/EscapedText";
import { RelativeTime } from "@/ui/RelativeTime";
import { StatusChipForToken } from "@/ui/StatusChip";

function effect(outcome: string, pushed: boolean): string {
  if (outcome !== "accepted") return "nothing pushed";
  return pushed ? "pushed to the pull request" : "accepted, not pushed yet";
}

/**
 * A ticket's PR-review corrective rounds: the one under way, then each that
 * ended, newest first, with its outcome, what reached the pull request, the
 * cause of a failure and its run. Without this a round was invisible on the
 * request: the card kept saying "ready for review" while a build ran on the
 * branch and after it was quarantined. Renders nothing for a ticket with no
 * rounds.
 */
export function ReviewRounds({ ticket }: { readonly ticket: RequestTicket }) {
  const ended = [...ticket.reviewRounds].reverse();
  if (ended.length === 0 && ticket.activeRoundRunId === "") return null;
  const link = "text-accent underline underline-offset-2";
  return (
    <div data-testid={`ticket-rounds-${ticket.index}`} className="flex flex-col gap-1.5">
      <h4 className="text-fg-muted text-xs font-semibold">Corrective rounds</h4>
      <ul className="flex flex-col gap-1.5 text-sm">
        {ticket.activeRoundRunId === "" ? null : (
          <li className="flex flex-wrap items-center gap-2">
            <span>{`Round ${ticket.reviewRounds.length + 1}`}</span>
            <StatusChipForToken token="slice_running" />
            <span className="text-fg-muted">building on the pull request's branch</span>
            <Link to={runPath(ticket.activeRoundRunId)} className={link}>
              Follow the run
            </Link>
          </li>
        )}
        {ended.map((round) => (
          <li key={round.index} className="flex flex-col gap-0.5">
            <div className="flex flex-wrap items-center gap-2">
              <span>{`Round ${round.index}`}</span>
              <StatusChipForToken token={round.outcome} />
              <span className="text-fg-muted">{effect(round.outcome, round.pushed)}</span>
              {round.at === "" ? null : (
                <span className="text-fg-subtle text-xs">
                  <RelativeTime value={round.at} />
                </span>
              )}
              {round.runId === "" ? null : (
                <Link to={runPath(round.runId)} className={link}>
                  {`Round ${round.index} run`}
                </Link>
              )}
            </div>
            {round.error === "" ? null : (
              // The run's own cause: machine text, shown as text.
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
