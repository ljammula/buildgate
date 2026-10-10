import { formatMedianAcceptedTokens } from "@/domain/cost";
import { Link } from "react-router";

import { projectReleasePath, projectStatsPath } from "@/routes/paths";
import { Card } from "@/ui/Card";
import { DescriptionItem, DescriptionList } from "@/ui/DescriptionList";
import { KillSwitchChip } from "@/ui/StatusChip";

import type { ProjectOpsRow } from "./useProjectOps";

export interface ProjectOpsCardProps {
  readonly row: ProjectOpsRow;
}

/** One project: its kill switch and its own recorded stats. */
export function ProjectOpsCard({ row }: ProjectOpsCardProps) {
  const { stats } = row;
  // A failed release fetch is "unknown", never "clear": an operator seeing
  // "clear" must be able to trust the switch was confirmed off (found via
  // review, PR #64).
  const engaged = row.release === null ? null : row.release.killSwitch.engaged;
  const causes = stats === null ? [] : Object.entries(stats.quarantinedByCause);
  return (
    <Card className="flex flex-col gap-2 p-4">
      <div className="flex items-center justify-between gap-3">
        <h2 className="font-mono text-sm font-semibold text-fg">{row.summary.project}</h2>
        <KillSwitchChip engaged={engaged} />
      </div>
      <p className="text-xs">
        <Link to={projectStatsPath(row.summary.project)} className="text-accent hover:underline">
          Stats
        </Link>
        {" · "}
        <Link to={projectReleasePath(row.summary.project)} className="text-accent hover:underline">
          Release
        </Link>
      </p>
      {stats === null ? (
        <p className="text-sm text-fg-muted">Stats unavailable for this project.</p>
      ) : (
        <DescriptionList>
          <DescriptionItem label="Accepted">
            {stats.accepted} / {stats.totalRuns} runs
            {stats.overrideRatePercent !== null
              ? `: ${stats.overrideRatePercent}% via override`
              : ""}
          </DescriptionItem>
          {causes.length > 0 ? (
            <DescriptionItem label="Quarantined by cause">
              {causes.map(([cause, count]) => `${cause} (${count})`).join(", ")}
            </DescriptionItem>
          ) : null}
          {stats.medianAcceptedTokens !== null ? (
            <DescriptionItem label="Median accepted">
              {formatMedianAcceptedTokens(stats.medianAcceptedTokens)}
            </DescriptionItem>
          ) : null}
        </DescriptionList>
      )}
    </Card>
  );
}
