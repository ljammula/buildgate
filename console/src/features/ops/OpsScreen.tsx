import { RefreshCw } from "lucide-react";

import { IconButton } from "@/ui/IconButton";
import { Callout, EmptyState, Spinner } from "@/ui/Feedback";
import { ErrorCallout } from "@/ui/ErrorDisplay";
import { PageBody, PageHeader } from "@/ui/PageLayout";

import { DaemonStatusCard } from "./DaemonStatusCard";
import { ProjectOpsCard } from "./ProjectOpsCard";
import { useProjectOps } from "./useProjectOps";

/**
 * Cross-project operations view: every project's quarantine breakdown,
 * override rate and kill-switch state. Strictly observability: nothing here
 * mutates anything.
 */
export function OpsScreen() {
  const { projects, rows, loading, startTokenFailure, refresh } = useProjectOps();

  let body;
  if (projects.isError && projects.data === undefined) {
    body = (
      <div>
        <p className="mb-2 text-sm text-fg">Could not load operations view.</p>
        <ErrorCallout error={projects.error} startClass onRetry={refresh} />
      </div>
    );
  } else if (loading) {
    body = <Spinner />;
  } else if (rows.length === 0) {
    body = <EmptyState title="No projects recorded yet." />;
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
            link `factoryd serve` printed in its own log/terminal output just now (ends in `#t=...`)
            -- the start token changes every restart.
          </Callout>
        ) : null}
        <div className="flex flex-col gap-3">
          {rows.map((row) => (
            <ProjectOpsCard key={row.summary.project} row={row} />
          ))}
        </div>
      </>
    );
  }

  return (
    <>
      <PageHeader
        title="Operations"
        actions={
          <IconButton label="Refresh" onClick={() => void refresh()}>
            <RefreshCw aria-hidden="true" />
          </IconButton>
        }
      />
      <PageBody>
        <div className="flex flex-col gap-3">
          <DaemonStatusCard />
          {body}
        </div>
      </PageBody>
    </>
  );
}
