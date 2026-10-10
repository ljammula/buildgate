import { useLocation, useNavigate } from "react-router";

import { useApi } from "@/api/ApiProvider";
import { useRun } from "@/api/runQueries";
import { DiffPanel } from "@/features/run-detail/DiffPanel";
import { RunHeader } from "@/features/run-detail/RunHeader";
import { ReleasePanel } from "@/features/run-detail/ReleasePanel";
import { RunOverview } from "@/features/run-detail/RunOverview";
import { useRunProgress } from "@/features/run-detail/useRunProgress";
import { parseRunView, runPath, runViewPath } from "@/routes/paths";
import { ErrorCallout } from "@/ui/ErrorDisplay";
import { Callout, Spinner } from "@/ui/Feedback";
import { PageBody, PageHeader } from "@/ui/PageLayout";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/ui/Tabs";

/**
 * One run: its Overview, and its diff and release decision as views selected
 * by the URL (`?view=diff|release`), so a reload or a shared link lands on
 * the same view. A view's data is fetched only while it is selected (the
 * Overview's Release card alone asks for an accepted run's decision, to show
 * its verdict); the progress feed lives here so it survives a switch of view.
 */
export function RunPage({ id }: { id: string }) {
  const { config } = useApi();
  const { query, streamError } = useRun(id);
  const progress = useRunProgress(id);
  const view = parseRunView(useLocation().search);
  const navigate = useNavigate();

  const run = query.data;
  if (run === undefined) {
    return (
      <>
        <PageHeader title="Run detail" />
        <PageBody>
          {query.isError ? (
            <ErrorCallout
              error={query.error}
              onRetry={async () => {
                await query.refetch();
              }}
            />
          ) : (
            <Spinner label="Loading run" />
          )}
        </PageBody>
      </>
    );
  }

  return (
    <>
      <RunHeader run={run} />
      <PageBody>
        {query.isError ? (
          <Callout tone="warning">
            Showing the last loaded run. The refresh failed, so the page may be stale.
          </Callout>
        ) : null}
        <Tabs
          value={view ?? "overview"}
          onValueChange={(next) => {
            void navigate(
              next === "diff" || next === "release" ? runViewPath(id, next) : runPath(id),
            );
          }}
        >
          <TabsList>
            <TabsTrigger value="overview">Overview</TabsTrigger>
            {run.diffAvailable || view === "diff" ? (
              <TabsTrigger value="diff">Diff</TabsTrigger>
            ) : null}
            <TabsTrigger value="release">Release</TabsTrigger>
          </TabsList>
          <TabsContent value="overview">
            <RunOverview
              run={run}
              progress={progress}
              streamError={streamError}
              temporalUiUrl={config.temporalUiUrl}
            />
          </TabsContent>
          <TabsContent value="diff">
            <DiffPanel runId={id} />
          </TabsContent>
          <TabsContent value="release">
            <ReleasePanel runId={id} />
          </TabsContent>
        </Tabs>
      </PageBody>
    </>
  );
}
