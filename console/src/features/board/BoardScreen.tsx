import { RefreshCw } from "lucide-react";
import { useEffect, useMemo, useState } from "react";

import { useApi } from "@/api/ApiProvider";
import { queueRunPollMs } from "@/api/polling";
import { useRequestBoard } from "@/api/requestQueries";
import { useQueueRunStatus } from "@/api/runQueries";
import { activityLimit, recentActivity } from "@/domain/activity";
import { buildBoard, showsNeedsYou, visibleColumns } from "@/domain/boardColumns";
import {
  applyBoardWindow,
  boardWindowLabel,
  instantInBoardWindow,
  scopeLabel,
} from "@/domain/boardWindow";
import { distinctProjects, matchesRequestBoardFilters } from "@/domain/boardFilters";
import { factoryHealth } from "@/domain/health";
import { needsHumanCount, sortedRequests } from "@/domain/requestOrder";
import { sectionForRequest, type RequestBoardSection } from "@/domain/boardFilters";
import { updateNeedsHumanSignal } from "@/platform/tabTitle";
import { Button } from "@/ui/Button";
import { EmptyState, Spinner } from "@/ui/Feedback";
import { ErrorCallout } from "@/ui/ErrorDisplay";
import { PageBody, PageHeader } from "@/ui/PageLayout";
import { useNow } from "@/ui/Time";

import { ActivityPanel } from "./ActivityPanel";
import { BoardStrips } from "./BoardStrips";
import { BoardToolbar } from "./BoardToolbar";
import { HealthStrip } from "./HealthStrip";
import { KanbanBoard } from "./KanbanBoard";
import { NumbersPanel } from "./NumbersPanel";
import { OlderHiddenNote } from "./OlderHiddenNote";
import { RequestSection } from "./RequestSection";
import { ViewToggle } from "./ViewToggle";
import { queueRunWarning } from "./boardModel";
import { useBoardFilters } from "./useBoardFilters";
import { useBoardFreshness } from "./useBoardFreshness";
import { useBoardView } from "./useBoardView";
import { useCollapsedLanes } from "./useCollapsedLanes";

const SECTION_ORDER: readonly RequestBoardSection[] = ["needsYou", "working", "finished"];

/**
 * Mission Control, the home screen at /: the factory's health, every request
 * as a board (five columns by who has to act, a lane per project) or as the
 * three-section list, the numbers and the latest activity. It is laid out to
 * fit the window: the strips and the toolbar stay put, and the board, the
 * list, the numbers and the activity each scroll inside their own box.
 *
 * The Board/List choice is kept per browser; the URL's query string filters
 * both views (`group=` narrows the board to that section's columns, `days=`
 * is the window on finished work, the activity and the numbers). A failed
 * refresh keeps the last list under a warning; the tab title carries the
 * needs-you count, or `(?)` after a failed poll.
 */
export function BoardScreen() {
  const { config, canWrite } = useApi();
  const { query, live, streamError, failedAttempts } = useRequestBoard();
  const queueRun = useQueueRunStatus(queueRunPollMs);
  const controls = useBoardFilters();
  const freshness = useBoardFreshness(live, streamError, failedAttempts);
  const now = useNow(30_000);
  const boardView = useBoardView();
  const { view } = boardView;
  const collapsedLanes = useCollapsedLanes();
  const [showCancelled, setShowCancelled] = useState(false);

  const requests = query.data;
  const refreshError = query.error;
  const needsHuman = requests === undefined ? null : needsHumanCount(requests);
  const pollFailed = refreshError !== null;

  // Runs after a failed poll too: the tab title must show `(?)` at once, not
  // keep a count that may now be wrong.
  useEffect(() => {
    updateNeedsHumanSignal(needsHuman, pollFailed);
  }, [needsHuman, pollFailed]);

  const { days } = controls.filters;
  const matching = useMemo(
    () => (requests ?? []).filter((r) => matchesRequestBoardFilters(r, controls.filters)),
    [requests, controls.filters],
  );
  // The window only ever takes finished requests out: what waits on the
  // factory or the operator is in view at any age.
  // The list always draws cancelled requests; the board only on request.
  const cancelledInView = view === "list" || showCancelled;
  const { shown: visible, olderHidden } = useMemo(
    () => applyBoardWindow(matching, days, now, cancelledInView),
    [matching, days, now, cancelledInView],
  );
  const sections = useMemo(() => {
    const sorted = sortedRequests(visible);
    return SECTION_ORDER.map((section) => ({
      section,
      requests: sorted.filter((r) => sectionForRequest(r) === section),
    })).filter((entry) => entry.requests.length > 0);
  }, [visible]);
  // Grouped once per list, filter or clock change (the clock ticks every
  // 30 s), never per render.
  const board = useMemo(() => buildBoard(visible, { showCancelled }), [visible, showCancelled]);
  const health = useMemo(
    () => factoryHealth(queueRun.data ?? null, requests ?? []),
    [queueRun.data, requests],
  );
  // Activity (and the numbers) follow the project filter and the window, not
  // the search box or the section filter: they are about the factory, not
  // about the cards in view. Moves outside the window are dropped before
  // the sort.
  const { projects } = controls.filters;
  const activity = useMemo(
    () =>
      recentActivity(
        (requests ?? []).filter((r) => projects.size === 0 || projects.has(r.project)),
        activityLimit,
        (at) => instantInBoardWindow(at, days, now),
      ),
    [requests, projects, days, now],
  );
  const showAllTime = (): void => {
    controls.selectDays("all");
  };

  const refresh = (): void => {
    void query.refetch();
  };
  const headerActions = (
    <>
      <ViewToggle view={view} onChange={boardView.choose} />
      <Button
        variant="ghost"
        size="icon"
        aria-label="Refresh"
        disabled={query.isFetching}
        onClick={refresh}
      >
        <RefreshCw aria-hidden="true" />
      </Button>
    </>
  );
  const allProjects = requests === undefined ? [] : distinctProjects(requests);

  return (
    // The screen is as tall as the window, so its boxes scroll and the page
    // does not. A short window keeps a usable board and scrolls the page; a
    // phone, where the sidebar sits on top, flows as a page.
    <div className="flex h-screen min-h-[42rem] flex-col max-md:h-auto max-md:min-h-0">
      <PageHeader title="Mission Control" actions={headerActions} />
      <PageBody className="min-h-0 flex-1 gap-4">
        {requests === undefined ? (
          refreshError === null ? (
            <Spinner />
          ) : (
            <ErrorCallout error={refreshError} />
          )
        ) : (
          <>
            <HealthStrip health={health} />
            <BoardStrips
              releasePolicyWarning={config.releasePolicyWarning}
              workerWarning={queueRunWarning(queueRun.data ?? null, requests, now)}
              onRetryWorker={() => void queueRun.refetch()}
              streamError={streamError}
              needsYouCount={needsHuman ?? 0}
              needsYouVisible={showsNeedsYou(visible)}
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
              allProjects={allProjects}
              freshness={freshness}
              lastUpdateAt={query.dataUpdatedAt}
              onSearch={controls.setSearch}
              onToggleSection={controls.toggleSection}
              onToggleProject={controls.toggleProject}
              onSelectDays={controls.selectDays}
            />
            {matching.length === 0 ? (
              <EmptyState title="No requests found." />
            ) : view === "board" ? (
              <KanbanBoard
                className="min-h-72 flex-1 max-md:h-[32rem] max-md:flex-none"
                board={board}
                columns={visibleColumns(controls.filters.section, board.counts)}
                showProject={allProjects.length > 1 && board.lanes.length <= 1}
                collapsedLanes={collapsedLanes}
                showCancelled={showCancelled}
                onShowCancelled={setShowCancelled}
                olderHidden={olderHidden}
                onShowAllTime={showAllTime}
                canWrite={canWrite}
                now={now}
                onShowAllDone={() => {
                  // For this visit: following a link is not choosing List.
                  boardView.showOnce("list");
                  controls.selectSection("finished");
                }}
              />
            ) : (
              // Focusable, so the keyboard can scroll it.
              <div
                role="group"
                aria-label="Request list"
                tabIndex={0}
                className="flex min-h-72 flex-1 flex-col gap-6 overflow-y-auto focus-visible:outline-2 max-md:flex-none"
              >
                {sections.map((entry) => (
                  <RequestSection
                    key={entry.section}
                    section={entry.section}
                    requests={entry.requests}
                    now={now}
                    showProject={allProjects.length > 1}
                  />
                ))}
                {/* The window applies to the Finished section only. */}
                <OlderHiddenNote count={olderHidden} onShowAllTime={showAllTime} />
              </div>
            )}
            <div className="grid shrink-0 items-stretch gap-4 xl:grid-cols-[minmax(0,2fr)_minmax(0,1fr)]">
              <NumbersPanel days={days} projects={projects} className="max-h-44" />
              <ActivityPanel
                entries={activity}
                windowLabel={scopeLabel(boardWindowLabel(days), projects)}
                className="max-h-44"
              />
            </div>
          </>
        )}
      </PageBody>
    </div>
  );
}
