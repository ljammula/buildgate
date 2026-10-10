import { Plus, RefreshCw } from "lucide-react";
import { Link } from "react-router";

import { projectsPath } from "@/routes/paths";
import { Button } from "@/ui/Button";
import { IconButton } from "@/ui/IconButton";
import { EmptyState, Spinner } from "@/ui/Feedback";
import { ErrorCallout } from "@/ui/ErrorDisplay";
import { PageBody, PageHeader } from "@/ui/PageLayout";
import { StaleWarning } from "@/ui/StaleWarning";
import { Table, TableBody, TableFrame, TableHead, TableHeaderCell, TableRow } from "@/ui/Table";

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
        title="Runs"
        actions={
          <>
            <Button asChild variant="primary" size="sm">
              <Link to={projectsPath()}>
                <Plus aria-hidden="true" />
                New run
              </Link>
            </Button>
            <IconButton label="Refresh" disabled={loading} onClick={() => void refresh()}>
              <RefreshCw aria-hidden="true" />
            </IconButton>
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
                <Table className="min-w-[70rem] table-fixed">
                  <TableHead>
                    <TableRow className="hover:bg-transparent">
                      <TableHeaderCell className="w-64">Run</TableHeaderCell>
                      <TableHeaderCell className="w-40">Run ID</TableHeaderCell>
                      <TableHeaderCell className="w-36">State</TableHeaderCell>
                      <TableHeaderCell className="w-48">Activity</TableHeaderCell>
                      <TableHeaderCell numeric className="w-24">
                        Elapsed
                      </TableHeaderCell>
                      <TableHeaderCell className="w-44">Created</TableHeaderCell>
                      <TableHeaderCell>Project</TableHeaderCell>
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
