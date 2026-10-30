import { Link } from "react-router";

import type { RequestTicket } from "@/domain/request";
import { runPath } from "@/routes/paths";
import { EscapedText } from "@/shared/text/EscapedText";
import { RelativeTime } from "@/ui/RelativeTime";
import { StatusChipForToken } from "@/ui/StatusChip";

function effect(outcome: string, pushed: boolean): string {
  if (outcome !== "accepted") return "nothing pushed";
  return pushed ? "the pull request now has the replayed commits" : "accepted, not pushed yet";
}

/**
 * A ticket's restacks: the one under way, then each that ended, newest first.
 * A restack replays the ticket's commits onto the new tip of the branch its
 * pull request stacks on, and a run judges them again with every gate and
 * review; nothing is rebuilt. Renders nothing for a ticket never restacked.
 */
export function Restacks({ ticket }: { readonly ticket: RequestTicket }) {
  const ended = [...ticket.restackRounds].reverse();
  const active = ticket.restack;
  if (ended.length === 0 && active === null) return null;
  const link = "text-accent underline underline-offset-2";
  return (
    <div data-testid={`ticket-restacks-${ticket.index}`} className="flex flex-col gap-1.5">
      <h4 className="text-fg-muted text-xs font-semibold">Restacks</h4>
      <ul className="flex flex-col gap-1.5 text-sm">
        {active === null ? null : (
          <li className="flex flex-wrap items-center gap-2">
            <span>{`Restack ${active.n}`}</span>
            <StatusChipForToken token="slice_running" />
            <span className="text-fg-muted">
              <EscapedText text={`replaying its commits onto ${active.ontoSha.slice(0, 12)}`} />
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
                  Restack run
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
