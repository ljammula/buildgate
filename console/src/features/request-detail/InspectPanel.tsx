import type { Inspection } from "@/domain/request";
import { CopyableCommand } from "@/ui/CopyableCommand";

import { Panel } from "./Panel";

export interface InspectPanelProps {
  readonly inspection: Inspection;
}

/**
 * The commands that show the work before a decision, as the server built
 * them (`factoryd inspect` prints the same): the drafted files at a review
 * gate, or each built ticket's commits. They run in a shell on the machine
 * that runs the factory.
 */
export function InspectPanel({ inspection }: InspectPanelProps) {
  return (
    <Panel title="Commands to look at the work" testId="inspect-panel">
      <p className="text-xs text-fg-muted">
        Run these in a shell on the machine that runs the factory.
      </p>
      {inspection.commands.map((c) => (
        <CopyableCommand key={c.command} label={c.label} command={c.command} />
      ))}
      {inspection.tickets.map((t) => (
        <section key={t.runId} className="flex flex-col gap-2" aria-label={ticketHeading(t.ticket)}>
          <h3 className="text-sm font-medium text-fg">{ticketHeading(t.ticket)}</h3>
          {t.worktree === "" ? null : (
            <p className="text-xs text-fg-muted">
              Worktree <code className="font-mono break-all">{t.worktree}</code>
            </p>
          )}
          {t.commands.map((c) => (
            <CopyableCommand key={c.command} label={c.label} command={c.command} />
          ))}
        </section>
      ))}
      {inspection.note === "" ? null : <p className="text-xs text-fg-muted">{inspection.note}</p>}
    </Panel>
  );
}

function ticketHeading(ticket: number): string {
  return ticket > 0 ? `Ticket ${String(ticket).padStart(3, "0")}` : "Run";
}
