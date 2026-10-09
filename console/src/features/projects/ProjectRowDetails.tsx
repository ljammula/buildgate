import { TableCell, TableRow } from "@/ui/Table";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/ui/Tabs";

import { ProjectObservationsPanel } from "./ProjectObservationsPanel";
import { ProjectReleasePanel } from "./ProjectReleasePanel";
import { ProjectStatsPanel } from "./ProjectStatsPanel";

/**
 * The row under an opened project: its figures, its kill switch and what its
 * runs have shown, as tabs. Rendered only while the row is open, and a tab's panel only while it
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
            <TabsTrigger value="observations">Observations</TabsTrigger>
          </TabsList>
          <TabsContent value="stats" className="flex flex-col gap-3">
            <ProjectStatsPanel project={project} withForm={false} />
          </TabsContent>
          <TabsContent value="release" className="flex flex-col gap-3">
            <ProjectReleasePanel project={project} withForm={false} />
          </TabsContent>
          <TabsContent value="observations" className="flex flex-col gap-3">
            <ProjectObservationsPanel project={project} withForm={false} />
          </TabsContent>
        </Tabs>
      </TableCell>
    </TableRow>
  );
}
