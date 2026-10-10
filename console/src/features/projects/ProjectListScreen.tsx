import { ChevronDown, ChevronRight, Plus } from "lucide-react";
import { Fragment, useState } from "react";
import { Link } from "react-router";

import { useProjects } from "@/api/runQueries";
import { newRunPath } from "@/routes/paths";
import { Button } from "@/ui/Button";
import { IconButton } from "@/ui/IconButton";
import { EmptyState, Spinner } from "@/ui/Feedback";
import { ErrorCallout } from "@/ui/ErrorDisplay";
import { PageBody, PageHeader } from "@/ui/PageLayout";
import {
  Table,
  TableBody,
  TableCell,
  TableFrame,
  TableHead,
  TableHeaderCell,
  TableRow,
} from "@/ui/Table";
import { LocalTimeText } from "@/ui/Time";

import { ProjectRowDetails } from "./ProjectRowDetails";

/**
 * Every project already run against (derived from run history), so another
 * run does not mean retyping its paths. A row opens the new-run form
 * pre-filled through the query string (see NewRunScreen); "Custom run" opens
 * it blank, for a project never run before.
 */
export function ProjectListScreen() {
  const projects = useProjects();
  const [expandedProject, setExpandedProject] = useState<string | null>(null);

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
          <TableFrame>
            <Table className="table-fixed">
              <TableHead>
                <TableRow className="hover:bg-transparent">
                  <TableHeaderCell>Project</TableHeaderCell>
                  <TableHeaderCell className="w-56">Kill-switch id</TableHeaderCell>
                  <TableHeaderCell className="w-24">Runs</TableHeaderCell>
                  <TableHeaderCell className="w-44">Last run</TableHeaderCell>
                  <TableHeaderCell className="w-32">
                    <span className="sr-only">Details</span>
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
                  const isExpanded = expandedProject === project.project;

                  return (
                    <Fragment key={project.projectPath}>
                      <TableRow>
                        <TableCell className="font-mono text-xs">
                          <Link
                            title={project.projectPath}
                            to={`${newRunPath()}?${quickFill.toString()}`}
                            className="block truncate text-accent hover:underline"
                          >
                            {project.projectPath}
                          </Link>
                        </TableCell>
                        <TableCell className="truncate font-mono text-xs" title={project.project}>
                          {project.project}
                        </TableCell>
                        <TableCell>
                          {project.runCount} run{project.runCount === 1 ? "" : "s"}
                        </TableCell>
                        <TableCell className="text-xs whitespace-nowrap tabular-nums">
                          <LocalTimeText value={project.lastRunAt} />
                        </TableCell>
                        <TableCell className="text-right">
                          <IconButton
                            aria-expanded={isExpanded}
                            label={`${isExpanded ? "Hide" : "Show"} details for ${project.projectPath}`}
                            onClick={() => {
                              setExpandedProject(isExpanded ? null : project.project);
                            }}
                          >
                            {isExpanded ? (
                              <ChevronDown aria-hidden="true" />
                            ) : (
                              <ChevronRight aria-hidden="true" />
                            )}
                          </IconButton>
                        </TableCell>
                      </TableRow>
                      {isExpanded ? <ProjectRowDetails project={project.project} /> : null}
                    </Fragment>
                  );
                })}
              </TableBody>
            </Table>
          </TableFrame>
        )}
      </PageBody>
    </>
  );
}
