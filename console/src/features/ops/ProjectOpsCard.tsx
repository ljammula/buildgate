import { formatMedianAcceptedTokens } from "@/domain/cost";
import { Link } from "react-router";

import { projectReleasePath, projectStatsPath } from "@/routes/paths";
import { Card } from "@/ui/Card";
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
        <div className="flex flex-col gap-1 text-sm text-fg">
          <p>
            Accepted {stats.accepted} / {stats.totalRuns} runs
            {stats.overrideRatePercent !== null
              ? `: ${stats.overrideRatePercent}% via override`
              : ""}
          </p>
          {causes.length > 0 ? (
            <p>
              Quarantined by cause:{" "}
              {causes.map(([cause, count]) => `${cause} (${count})`).join(", ")}
            </p>
          ) : null}
          {stats.medianAcceptedTokens !== null ? (
            <p>Median accepted: {formatMedianAcceptedTokens(stats.medianAcceptedTokens)}</p>
          ) : null}
        </div>
      )}
    </Card>
  );
}
