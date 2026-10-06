import { Play, Plus } from "lucide-react";
import { Link } from "react-router";

import { useProjects } from "@/api/runQueries";
import { newRunPath } from "@/routes/paths";
import { Button } from "@/ui/Button";
import { EmptyState, Spinner } from "@/ui/Feedback";
import { ErrorCallout } from "@/ui/ErrorDisplay";
import { PageBody, PageHeader } from "@/ui/PageLayout";
import { Table, TableBody, TableCell, TableHead, TableHeaderCell, TableRow } from "@/ui/Table";
import { LocalTimeText } from "@/ui/Time";

/**
 * Every project already run against (derived from run history), so another
 * run does not mean retyping its paths. A row opens the new-run form
 * pre-filled through the query string (see NewRunScreen); "Custom run" opens
 * it blank, for a project never run before.
 */
export function ProjectListScreen() {
  const projects = useProjects();
  return (
    <>
      <PageHeader
        title="Projects"
        actions={
          <Button asChild variant="primary" size="sm">
            <Link to={newRunPath()}>
              <Plus aria-hidden="true" />
              Custom run
            </Link>
          </Button>
        }
      />
      <PageBody>
        {projects.data === undefined ? (
          projects.error === null ? (
            <Spinner />
          ) : (
            <ErrorCallout error={projects.error} onRetry={() => void projects.refetch()} />
          )
        ) : projects.data.length === 0 ? (
          <EmptyState title="No projects yet. Start a custom run to create the first one." />
        ) : (
          <Table>
            <TableHead>
              <TableRow className="hover:bg-transparent">
                <TableHeaderCell>Project</TableHeaderCell>
                <TableHeaderCell>Kill-switch id</TableHeaderCell>
                <TableHeaderCell>Runs</TableHeaderCell>
                <TableHeaderCell>Last run</TableHeaderCell>
                <TableHeaderCell>
                  <span className="sr-only">Start a run</span>
                </TableHeaderCell>
              </TableRow>
            </TableHead>
            <TableBody>
              {projects.data.map((project) => {
                const quickFill = new URLSearchParams({
                  workspace: project.workspacePath,
                  spec: project.specPath,
                  repository: project.repository,
                });
                return (
                  <TableRow key={project.projectPath}>
                    <TableCell className="font-mono text-xs">
                      <Link
                        to={`${newRunPath()}?${quickFill.toString()}`}
                        className="text-accent hover:underline"
                      >
                        {project.projectPath}
                      </Link>
                    </TableCell>
                    <TableCell className="font-mono text-xs">{project.project}</TableCell>
                    <TableCell>
                      {project.runCount} run{project.runCount === 1 ? "" : "s"}
                    </TableCell>
                    <TableCell className="text-xs">
                      <LocalTimeText value={project.lastRunAt} />
                    </TableCell>
                    <TableCell className="text-right text-fg-muted">
                      <Play aria-hidden="true" className="inline size-4" />
                    </TableCell>
                  </TableRow>
                );
              })}
            </TableBody>
          </Table>
        )}
      </PageBody>
    </>
  );
}
