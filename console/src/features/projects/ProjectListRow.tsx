import { ChevronDown, ChevronRight } from "lucide-react";
import { Link } from "react-router";

import { acceptedText } from "@/domain/project";
import { shortPath } from "@/domain/middleTruncate";
import { NO_VALUE } from "@/domain/noValue";
import { newRunPath } from "@/routes/paths";
import { IconButton } from "@/ui/IconButton";
import { RelativeTime } from "@/ui/RelativeTime";
import { KillSwitchChip } from "@/ui/StatusChip";
import { TableCell, TableRow } from "@/ui/Table";

import type { ProjectRow } from "./useProjectRows";

export interface ProjectListRowProps {
  readonly row: ProjectRow;
  readonly expanded: boolean;
  readonly onToggle: () => void;
}

/** One project at a glance: its runs, acceptance and kill switch, and the button that opens the rest. */
export function ProjectListRow({ row, expanded, onToggle }: ProjectListRowProps) {
  const { summary, stats, release } = row;
  const quickFill = new URLSearchParams({
    workspace: summary.workspacePath,
    spec: summary.specPath,
    repository: summary.repository,
  });
  // A failed release read is "unknown", never "clear": an operator seeing
  // "clear" must be able to trust the switch was confirmed off.
  const engaged = release === null ? null : release.killSwitch.engaged;
  const pending = row.loading;
  return (
    <TableRow>
      <TableCell className="truncate font-mono text-xs" title={summary.project}>
        {summary.project}
      </TableCell>
      <TableCell className="font-mono text-xs">
        <Link
          title={summary.projectPath}
          to={`${newRunPath()}?${quickFill.toString()}`}
          className="block truncate text-accent hover:underline"
        >
          {shortPath(summary.projectPath)}
        </Link>
      </TableCell>
      <TableCell numeric>{summary.runCount}</TableCell>
      <TableCell className="text-xs">
        {pending ? (
          NO_VALUE
        ) : row.statsFailed ? (
          <span title="Stats could not be read">{NO_VALUE}</span>
        ) : (
          acceptedText(stats)
        )}
      </TableCell>
      <TableCell>
        {pending ? (
          NO_VALUE
        ) : row.releaseFailed ? (
          <span title="Last read failed; showing nothing rather than an old state">
            <KillSwitchChip engaged={null} />
          </span>
        ) : (
          <KillSwitchChip engaged={engaged} />
        )}
      </TableCell>
      <TableCell className="text-xs whitespace-nowrap tabular-nums">
        <RelativeTime value={summary.lastRunAt} />
      </TableCell>
      <TableCell className="text-right">
        <IconButton
          aria-expanded={expanded}
          label={`${expanded ? "Hide" : "Show"} details for ${summary.projectPath}`}
          onClick={onToggle}
        >
          {expanded ? <ChevronDown aria-hidden="true" /> : <ChevronRight aria-hidden="true" />}
        </IconButton>
      </TableCell>
    </TableRow>
  );
}
