import { Link } from "react-router";

import { runIsTerminalForDisplay } from "@/domain/run";
import type { Run } from "@/domain/run";
import type { RequestSummary } from "@/domain/request";
import { stageLabels } from "@/domain/runDetail";
import { runPath } from "@/routes/paths";
import { CompactId } from "@/ui/CompactId";
import { TableCell, TableRow } from "@/ui/Table";
import { ShortPath } from "@/ui/ShortPath";
import { StatusChipForToken } from "@/ui/StatusChip";
import { RelativeTime } from "@/ui/RelativeTime";
import { ElapsedText, StallChip } from "@/ui/Time";

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
      <TableCell>
        <Link
          to={runPath(run.id)}
          className="block truncate font-medium text-accent hover:underline"
          title={request === null ? run.ticket : request.title}
        >
          {request === null ? run.ticket : request.title}
        </Link>
        {request === null ? null : <div className="text-xs text-fg-muted">Ticket {run.ticket}</div>}
        <StallChip run={run} />
      </TableCell>
      <TableCell className="text-xs">
        <CompactId value={run.id} max={24} label={`run id ${run.id}`} />
      </TableCell>
      <TableCell>
        <StatusChipForToken token={run.state} />
      </TableCell>
      <TableCell className="text-xs text-fg-muted">
        {terminal ? null : (
          <span>
            {run.currentStage === null || run.currentStage === ""
              ? "starting"
              : Object.hasOwn(stageLabels, run.currentStage)
                ? stageLabels[run.currentStage]
                : run.currentStage}
            {" · last activity "}
            <ElapsedText since={run.lastProgressAt ?? run.createdAt} className="font-mono" /> ago
          </span>
        )}
      </TableCell>
      <TableCell numeric className="font-mono text-xs">
        <ElapsedText since={run.createdAt} until={terminal ? run.updatedAt : null} />
      </TableCell>
      <TableCell className="text-xs whitespace-nowrap tabular-nums">
        <RelativeTime value={run.createdAt} />
      </TableCell>
      <TableCell>
        <ShortPath path={run.projectPath} />
      </TableCell>
    </TableRow>
  );
}
