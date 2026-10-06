import { RefreshCw } from "lucide-react";
import { useEffect, useMemo } from "react";

import { useApi } from "@/api/ApiProvider";
import { useRequestBoard } from "@/api/requestQueries";
import { useQueueRunStatus } from "@/api/runQueries";
import { distinctProjects, matchesRequestBoardFilters } from "@/domain/boardFilters";
import { needsHumanCount, sortedRequests } from "@/domain/requestOrder";
import { sectionForRequest, type RequestBoardSection } from "@/domain/boardFilters";
import { updateNeedsHumanSignal } from "@/platform/tabTitle";
import { Button } from "@/ui/Button";
import { EmptyState, Spinner } from "@/ui/Feedback";
import { ErrorCallout } from "@/ui/ErrorDisplay";
import { PageBody, PageHeader } from "@/ui/PageLayout";
import { useNow } from "@/ui/Time";

import { BoardStrips } from "./BoardStrips";
import { BoardToolbar } from "./BoardToolbar";
import { RequestSection } from "./RequestSection";
import { queueRunWarning } from "./boardModel";
import { useBoardFilters } from "./useBoardFilters";
import { useBoardFreshness } from "./useBoardFreshness";

const SECTION_ORDER: readonly RequestBoardSection[] = ["needsYou", "working", "finished"];

/**
 * The request board at /: every request in three sections (Needs you /
 * Working / Finished), filtered by the URL's query string. A failed refresh
 * keeps the last list under a warning; the tab title carries the needs-you
 * count, or `(?)` after a failed poll.
 */
export function BoardScreen() {
  const { config } = useApi();
  const { query, live, streamError, failedAttempts } = useRequestBoard();
  const queueRun = useQueueRunStatus();
  const controls = useBoardFilters();
  const freshness = useBoardFreshness(live, streamError, failedAttempts);
  const now = useNow(30_000);

  const requests = query.data;
  const refreshError = query.error;
  const needsHuman = requests === undefined ? null : needsHumanCount(requests);
  const pollFailed = refreshError !== null;

  // Runs after a failed poll too: the tab title must show `(?)` at once, not
  // keep a count that may now be wrong.
  useEffect(() => {
    updateNeedsHumanSignal(needsHuman, pollFailed);
  }, [needsHuman, pollFailed]);

  const sections = useMemo(() => {
    const sorted = sortedRequests(
      (requests ?? []).filter((r) => matchesRequestBoardFilters(r, controls.filters)),
    );
    return SECTION_ORDER.map((section) => ({
      section,
      requests: sorted.filter((r) => sectionForRequest(r) === section),
    })).filter((entry) => entry.requests.length > 0);
  }, [requests, controls.filters]);

  const refresh = (): void => {
    void query.refetch();
  };
  const refreshButton = (
    <Button
      variant="ghost"
      size="icon"
      aria-label="Refresh"
      disabled={query.isFetching}
      onClick={refresh}
    >
      <RefreshCw aria-hidden="true" />
    </Button>
  );

  return (
    <>
      <PageHeader title="Requests" actions={refreshButton} />
      <PageBody>
        {requests === undefined ? (
          refreshError === null ? (
            <Spinner />
          ) : (
            <ErrorCallout error={refreshError} />
          )
        ) : (
          <>
            <BoardStrips
              releasePolicyWarning={config.releasePolicyWarning}
              workerWarning={queueRunWarning(queueRun.data ?? null, requests, now)}
              onRetryWorker={() => void queueRun.refetch()}
              streamError={streamError}
              needsYouCount={needsHuman ?? 0}
              needsYouSelected={controls.filters.section === "needsYou"}
              onViewNeedsYou={() => {
                controls.selectSection("needsYou");
              }}
              refreshError={refreshError}
              refreshing={query.isFetching}
              onRetryRefresh={refresh}
            />
            <BoardToolbar
              filters={controls.filters}
              searchText={controls.searchText}
              allProjects={distinctProjects(requests)}
              freshness={freshness}
              lastUpdateAt={query.dataUpdatedAt}
              onSearch={controls.setSearch}
              onToggleSection={controls.toggleSection}
              onToggleProject={controls.toggleProject}
            />
            {sections.length === 0 ? (
              <EmptyState title="No requests found." />
            ) : (
              sections.map((entry) => (
                <RequestSection
                  key={entry.section}
                  section={entry.section}
                  requests={entry.requests}
                  now={now}
                />
              ))
            )}
          </>
        )}
      </PageBody>
    </>
  );
}
