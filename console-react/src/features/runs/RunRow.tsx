import { Link } from "react-router";

import { runIsTerminalForDisplay } from "@/domain/run";
import type { Run } from "@/domain/run";
import type { RequestSummary } from "@/domain/request";
import { runPath } from "@/routes/paths";
import { TableCell, TableRow } from "@/ui/Table";
import { StatusChipForToken } from "@/ui/StatusChip";
import { ElapsedText, LocalTimeText, StallChip } from "@/ui/Time";

export interface RunRowProps {
  readonly run: Run;
  /** The run's request when GET /requests resolved it: its title replaces the raw ticket id. */
  readonly request: RequestSummary | null;
}

/**
 * One run. Elapsed counts from creation to the last update once the run is
 * terminal for display (quarantined included: see runIsTerminalForDisplay),
 * and ticks while it is not. A non-terminal run also shows its own stage and
 * the time since its last progress line ("silence is a bug"), so "still
 * verifying" reads differently from "nothing has happened in ten minutes".
 */
export function RunRow({ run, request }: RunRowProps) {
  const terminal = runIsTerminalForDisplay(run);
  return (
    <TableRow>
      <TableCell className="max-w-72">
        <Link to={runPath(run.id)} className="font-medium text-accent hover:underline">
          {request === null ? run.ticket : request.title}
        </Link>
        {request === null ? null : <div className="text-xs text-fg-muted">Ticket {run.ticket}</div>}
        <StallChip run={run} />
      </TableCell>
      <TableCell>
        <StatusChipForToken token={run.state} />
      </TableCell>
      <TableCell className="font-mono text-xs break-all">{run.projectPath}</TableCell>
      <TableCell className="font-mono text-xs">
        <ElapsedText since={run.createdAt} until={terminal ? run.updatedAt : null} />
      </TableCell>
      <TableCell className="text-xs text-fg-muted">
        {terminal ? null : (
          <span>
            {run.currentStage === null || run.currentStage === "" ? "starting" : run.currentStage}
            {" · last activity "}
            <ElapsedText since={run.lastProgressAt ?? run.createdAt} className="font-mono" /> ago
          </span>
        )}
      </TableCell>
      <TableCell className="text-xs">
        <LocalTimeText value={run.createdAt} />
      </TableCell>
      <TableCell className="font-mono text-xs">{run.id}</TableCell>
    </TableRow>
  );
}
