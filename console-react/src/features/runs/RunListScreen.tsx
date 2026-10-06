import { BarChart3, LayoutDashboard, Plus, RefreshCw, Shield } from "lucide-react";
import { Link } from "react-router";

import { opsPath, projectReleasePath, projectStatsPath, projectsPath } from "@/routes/paths";
import { Button } from "@/ui/Button";
import { EmptyState, Spinner } from "@/ui/Feedback";
import { ErrorCallout } from "@/ui/ErrorDisplay";
import { PageBody, PageHeader } from "@/ui/PageLayout";
import { StaleWarning } from "@/ui/StaleWarning";
import { Table, TableBody, TableFrame, TableHead, TableHeaderCell, TableRow } from "@/ui/Table";

import { ProjectLookupDialog } from "./ProjectLookupDialog";
import { RunRow } from "./RunRow";
import { useRunList } from "./useRunList";

/**
 * The run list, at /runs. Auto-refreshes; a failed refresh keeps the last
 * list under a warning rather than replacing it with an error (the last
 * loaded data is still meaningful), and the refresh controls are disabled
 * while a load is in flight.
 */
export function RunListScreen() {
  const { runs, requestsById, loading, refresh } = useRunList();

  return (
    <>
      <PageHeader
        title="Factory runs"
        actions={
          <>
            <Button asChild variant="primary" size="sm">
              <Link to={projectsPath()}>
                <Plus aria-hidden="true" />
                New run
              </Link>
            </Button>
            <ProjectLookupDialog
              title="Project release"
              pathFor={projectReleasePath}
              trigger={
                <Button
                  variant="ghost"
                  size="icon"
                  aria-label="Project release"
                  title="Project release"
                >
                  <Shield aria-hidden="true" />
                </Button>
              }
            />
            <ProjectLookupDialog
              title="Project stats"
              pathFor={projectStatsPath}
              trigger={
                <Button
                  variant="ghost"
                  size="icon"
                  aria-label="Project stats"
                  title="Project stats"
                >
                  <BarChart3 aria-hidden="true" />
                </Button>
              }
            />
            <Button asChild variant="ghost" size="icon" aria-label="Operations" title="Operations">
              <Link to={opsPath()}>
                <LayoutDashboard aria-hidden="true" />
              </Link>
            </Button>
            <Button
              variant="ghost"
              size="icon"
              aria-label="Refresh"
              title="Refresh"
              disabled={loading}
              onClick={() => void refresh()}
            >
              <RefreshCw aria-hidden="true" />
            </Button>
          </>
        }
      />
      <PageBody>
        {runs.data === undefined ? (
          runs.error === null ? (
            <Spinner />
          ) : (
            <ErrorCallout error={runs.error} />
          )
        ) : (
          <>
            {runs.error === null ? null : (
              <StaleWarning
                error={runs.error}
                retrying={loading}
                onRetry={() => void refresh()}
                testId="run-list-stale-banner"
              />
            )}
            {runs.data.length === 0 ? (
              <EmptyState title="No runs found." />
            ) : (
              <TableFrame>
                <Table className="min-w-[60rem] table-fixed">
                  <TableHead>
                    <TableRow className="hover:bg-transparent">
                      <TableHeaderCell className="w-[22%]">Run</TableHeaderCell>
                      <TableHeaderCell className="w-32">State</TableHeaderCell>
                      <TableHeaderCell className="w-64">Project</TableHeaderCell>
                      <TableHeaderCell className="w-24">Elapsed</TableHeaderCell>
                      <TableHeaderCell className="w-48">Activity</TableHeaderCell>
                      <TableHeaderCell className="w-44">Created</TableHeaderCell>
                      <TableHeaderCell className="w-44">Run ID</TableHeaderCell>
                    </TableRow>
                  </TableHead>
                  <TableBody>
                    {runs.data.map((run) => (
                      <RunRow
                        key={run.id}
                        run={run}
                        request={
                          run.requestId === "" ? null : (requestsById.get(run.requestId) ?? null)
                        }
                      />
                    ))}
                  </TableBody>
                </Table>
              </TableFrame>
            )}
          </>
        )}
      </PageBody>
    </>
  );
}
