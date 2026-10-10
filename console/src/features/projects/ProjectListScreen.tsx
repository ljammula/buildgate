import { Plus, RefreshCw } from "lucide-react";
import { Fragment } from "react";
import { Link, useSearchParams } from "react-router";

import { isProjectTab, newRunPath } from "@/routes/paths";
import { Button } from "@/ui/Button";
import { Callout, EmptyState, Spinner } from "@/ui/Feedback";
import { ErrorCallout } from "@/ui/ErrorDisplay";
import { IconButton } from "@/ui/IconButton";
import { PageBody, PageHeader } from "@/ui/PageLayout";
import { Table, TableBody, TableFrame, TableHead, TableHeaderCell, TableRow } from "@/ui/Table";

import { ProjectListRow } from "./ProjectListRow";
import { ProjectLookupForm } from "./ProjectLookupForm";
import { ProjectRowDetails } from "./ProjectRowDetails";
import { ProjectUnlistedPanel } from "./ProjectUnlistedPanel";
import { useProjectRows } from "./useProjectRows";

const columnCount = 7;

/**
 * The one place for a project's information: every project already run
 * against (derived from run history) with its runs, acceptance and kill
 * switch, and under a row's button its stats, release, trend, observations and
 * memory. Which row is open and on which tab is in the query string
 * (`?project=<id>&tab=<tab>`), so a link can open it. An id the list does not
 * hold (no run yet, but a kill switch can already be engaged) opens in a panel
 * above the table, and the lookup field in the header opens any id. A workspace link opens
 * the new-run form pre-filled (see NewRunScreen); "New run" opens it blank,
 * for a project never run before. Nothing here changes anything.
 */
export function ProjectListScreen() {
  const { projects, rows, startTokenFailure, refresh } = useProjectRows();
  const [params, setParams] = useSearchParams();
  const requested = params.get("project");
  // Rows are keyed by workspace; only the first row with the requested id opens.
  const openRow = rows.find((row) => row.summary.project === requested);
  const unlistedId =
    requested !== null && requested !== "" && projects.data !== undefined && openRow === undefined
      ? requested
      : null;
  // An unknown tab is ignored rather than shown as an error.
  const tabParam = params.get("tab");
  const tab = isProjectTab(tabParam) ? tabParam : "stats";

  const select = (project: string | null, nextTab: string | null) => {
    const next = new URLSearchParams(params);
    if (project === null) next.delete("project");
    else next.set("project", project);
    if (project === null || nextTab === null) next.delete("tab");
    else next.set("tab", nextTab);
    setParams(next, { replace: true });
  };

  const unlistedPanel =
    unlistedId !== null ? (
      <ProjectUnlistedPanel
        project={unlistedId}
        tab={tab}
        onTabChange={(next) => {
          select(unlistedId, next);
        }}
        onClose={() => {
          select(null, null);
        }}
      />
    ) : null;

  let body;
  if (projects.data === undefined) {
    body =
      projects.error === null ? (
        <Spinner />
      ) : (
        <ErrorCallout error={projects.error} onRetry={() => void refresh()} />
      );
  } else if (projects.data.length === 0) {
    body = <EmptyState title="No projects yet. Start a custom run to create the first one." />;
  } else {
    body = (
      <>
        {projects.isError ? (
          <Callout tone="warning" title="Showing the last loaded projects">
            The refresh failed, so this list may be out of date.
          </Callout>
        ) : null}
        {startTokenFailure ? (
          <Callout tone="danger">
            One or more projects&apos; stats/release could not be read (401/403). Open the console
            link <code className="font-mono">factoryd serve</code> printed in its own log/terminal
            output just now (ends in <code className="font-mono">#t=...</code>). The start token
            changes every restart.
          </Callout>
        ) : null}
        <TableFrame>
          <Table className="table-fixed">
            <TableHead>
              <TableRow className="hover:bg-transparent">
                <TableHeaderCell className="w-28">Project</TableHeaderCell>
                <TableHeaderCell>Workspace</TableHeaderCell>
                <TableHeaderCell numeric className="w-16">
                  Runs
                </TableHeaderCell>
                <TableHeaderCell className="w-36 xl:w-60">Accepted</TableHeaderCell>
                <TableHeaderCell className="w-44">Kill switch</TableHeaderCell>
                <TableHeaderCell className="w-24 xl:w-32">Last run</TableHeaderCell>
                <TableHeaderCell className="w-10">
                  <span className="sr-only">Details</span>
                </TableHeaderCell>
              </TableRow>
            </TableHead>
            <TableBody>
              {rows.map((row) => {
                const project = row.summary.project;
                const expanded = row === openRow;
                return (
                  <Fragment key={row.summary.projectPath}>
                    <ProjectListRow
                      row={row}
                      expanded={expanded}
                      onToggle={() => {
                        select(expanded ? null : project, null);
                      }}
                    />
                    {expanded ? (
                      <ProjectRowDetails
                        project={project}
                        tab={tab}
                        columns={columnCount}
                        onTabChange={(next) => {
                          select(project, next);
                        }}
                      />
                    ) : null}
                  </Fragment>
                );
              })}
            </TableBody>
          </Table>
        </TableFrame>
      </>
    );
  }

  return (
    <>
      <PageHeader
        title="Projects"
        actions={
          <>
            <ProjectLookupForm />
            <Button asChild variant="primary" size="sm">
              <Link to={newRunPath()}>
                <Plus aria-hidden="true" />
                New run
              </Link>
            </Button>
            <IconButton label="Refresh" onClick={() => void refresh()}>
              <RefreshCw aria-hidden="true" />
            </IconButton>
          </>
        }
      />
      <PageBody>
        <div className="flex flex-col gap-3">
          {unlistedPanel}
          {body}
        </div>
      </PageBody>
    </>
  );
}
