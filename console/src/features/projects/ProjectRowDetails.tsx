import type { ProjectTab } from "@/routes/paths";
import { TableCell, TableRow } from "@/ui/Table";

import { ProjectDetails } from "./ProjectDetails";

export interface ProjectRowDetailsProps {
  readonly project: string;
  readonly tab: ProjectTab;
  readonly onTabChange: (tab: ProjectTab) => void;
  readonly columns: number;
}

/** The table row under an opened project: its details across every column. */
export function ProjectRowDetails({ project, tab, onTabChange, columns }: ProjectRowDetailsProps) {
  return (
    <TableRow className="hover:bg-transparent">
      <TableCell colSpan={columns} className="bg-surface-sunken p-4">
        <ProjectDetails project={project} tab={tab} onTabChange={onTabChange} />
      </TableCell>
    </TableRow>
  );
}
