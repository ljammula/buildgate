import { type ProjectTab, isProjectTab } from "@/routes/paths";
import { TableCell, TableRow } from "@/ui/Table";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/ui/Tabs";

import { ProjectMemoryPanel } from "./ProjectMemoryPanel";
import { ProjectObservationsPanel } from "./ProjectObservationsPanel";
import { ProjectReleasePanel } from "./ProjectReleasePanel";
import { ProjectStatsPanel } from "./ProjectStatsPanel";
import { ProjectTrendPanel } from "./ProjectTrendPanel";

export interface ProjectRowDetailsProps {
  readonly project: string;
  readonly tab: ProjectTab;
  readonly onTabChange: (tab: ProjectTab) => void;
  readonly columns: number;
}

/**
 * The row under an opened project: its figures, its kill switch, how its numbers move, what its
 * runs have shown and its repository memory, as tabs. The selected tab comes from the URL, so a
 * link can open one. Rendered only while the row is open, and a tab's panel only while it
 * is the selected one, so a closed row reads nothing from the server.
 */
export function ProjectRowDetails({ project, tab, onTabChange, columns }: ProjectRowDetailsProps) {
  return (
    <TableRow className="hover:bg-transparent">
      <TableCell colSpan={columns} className="bg-surface-sunken p-4">
        <Tabs
          value={tab}
          onValueChange={(value) => {
            if (isProjectTab(value)) onTabChange(value);
          }}
        >
          <TabsList>
            <TabsTrigger value="stats">Stats</TabsTrigger>
            <TabsTrigger value="release">Release</TabsTrigger>
            <TabsTrigger value="trend">Trend</TabsTrigger>
            <TabsTrigger value="observations">Observations</TabsTrigger>
            <TabsTrigger value="memory">Memory</TabsTrigger>
          </TabsList>
          <TabsContent value="stats" className="flex flex-col gap-3">
            <ProjectStatsPanel project={project} />
          </TabsContent>
          <TabsContent value="release" className="flex flex-col gap-3">
            <ProjectReleasePanel project={project} />
          </TabsContent>
          <TabsContent value="trend" className="flex flex-col gap-3">
            <ProjectTrendPanel project={project} />
          </TabsContent>
          <TabsContent value="observations" className="flex flex-col gap-3">
            <ProjectObservationsPanel project={project} />
          </TabsContent>
          <TabsContent value="memory" className="flex flex-col gap-3">
            <ProjectMemoryPanel project={project} />
          </TabsContent>
        </Tabs>
      </TableCell>
    </TableRow>
  );
}
