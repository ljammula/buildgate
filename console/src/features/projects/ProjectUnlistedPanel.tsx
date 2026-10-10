import type { ProjectTab } from "@/routes/paths";
import { Button } from "@/ui/Button";
import { Card } from "@/ui/Card";

import { ProjectDetails } from "./ProjectDetails";

export interface ProjectUnlistedPanelProps {
  readonly project: string;
  readonly tab: ProjectTab;
  readonly onTabChange: (tab: ProjectTab) => void;
  readonly onClose: () => void;
}

/**
 * A project the list does not hold (it has no run yet), opened by its id. Its kill switch can
 * still be engaged, so its details are read all the same; a read that fails shows its error in
 * the tab as it does on a row.
 */
export function ProjectUnlistedPanel({
  project,
  tab,
  onTabChange,
  onClose,
}: ProjectUnlistedPanelProps) {
  return (
    <Card className="flex flex-col gap-3 p-4">
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0">
          <h2 className="truncate font-mono text-sm font-semibold text-fg" title={project}>
            {project}
          </h2>
          <p className="text-sm text-fg-muted">No runs recorded for this project.</p>
        </div>
        <Button size="sm" onClick={onClose}>
          Close
        </Button>
      </div>
      <ProjectDetails project={project} tab={tab} onTabChange={onTabChange} />
    </Card>
  );
}
