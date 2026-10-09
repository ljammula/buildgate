import { TableCell, TableRow } from "@/ui/Table";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/ui/Tabs";

import { ProjectMemoryPanel } from "./ProjectMemoryPanel";
import { ProjectObservationsPanel } from "./ProjectObservationsPanel";
import { ProjectReleasePanel } from "./ProjectReleasePanel";
import { ProjectStatsPanel } from "./ProjectStatsPanel";
import { ProjectTrendPanel } from "./ProjectTrendPanel";

/**
 * The row under an opened project: its figures, its kill switch, how its numbers move, what its
 * runs have shown and its repository memory, as tabs. Rendered only while the row is open, and a tab's panel only while it
 * is the selected one, so a closed row reads nothing from the server.
 */
export function ProjectRowDetails({ project }: { readonly project: string }) {
  return (
    <TableRow className="hover:bg-transparent">
      <TableCell colSpan={5} className="bg-surface-sunken p-4">
        <Tabs defaultValue="stats">
          <TabsList>
            <TabsTrigger value="stats">Stats</TabsTrigger>
            <TabsTrigger value="release">Release</TabsTrigger>
            <TabsTrigger value="trend">Trend</TabsTrigger>
            <TabsTrigger value="observations">Observations</TabsTrigger>
            <TabsTrigger value="memory">Memory</TabsTrigger>
          </TabsList>
          <TabsContent value="stats" className="flex flex-col gap-3">
            <ProjectStatsPanel project={project} withForm={false} />
          </TabsContent>
          <TabsContent value="release" className="flex flex-col gap-3">
            <ProjectReleasePanel project={project} withForm={false} />
          </TabsContent>
          <TabsContent value="trend" className="flex flex-col gap-3">
            <ProjectTrendPanel project={project} />
          </TabsContent>
          <TabsContent value="observations" className="flex flex-col gap-3">
            <ProjectObservationsPanel project={project} withForm={false} />
          </TabsContent>
          <TabsContent value="memory" className="flex flex-col gap-3">
            <ProjectMemoryPanel project={project} />
          </TabsContent>
        </Tabs>
      </TableCell>
    </TableRow>
  );
}
