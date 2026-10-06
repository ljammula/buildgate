import { RefreshCw } from "lucide-react";

import { useRunRelease } from "@/api/runQueries";
import { ReleaseDetails } from "@/features/run-detail/ReleaseDetails";
import { Button } from "@/ui/Button";
import { ErrorCallout } from "@/ui/ErrorDisplay";
import { Spinner } from "@/ui/Feedback";
import { StaleWarning } from "@/ui/StaleWarning";
import { Tooltip } from "@/ui/Tooltip";

/**
 * The release view of a run. GET /runs/{id}/release is start-token-gated, so
 * its errors carry the start-token guidance.
 *
 * On refresh the last loaded release stays visible, never replaced with a
 * spinner or blanked: a stale decision is still meaningful recorded evidence
 * (found via review: a screen left open across a run's acceptance or a
 * kill-switch transition kept showing "No decision recorded"). A refresh that
 * fails says so beside the retained data (found via review: it used to be
 * discarded silently, so a refresh right after a kill-switch transition could
 * keep presenting the old switch state as current). Refresh and Retry both
 * disable while a request is in flight (found via review: a second tap
 * started a concurrent request whose out-of-order response could overwrite a
 * newer snapshot and silently clear the warning).
 */
export function ReleasePanel({ runId }: { runId: string }) {
  const release = useRunRelease(runId);
  const loading = release.isFetching;

  let content;
  if (release.data !== undefined) {
    content = (
      <>
        {release.isError ? (
          <StaleWarning
            error={release.error}
            detail="next-step"
            startClass
            retrying={loading}
            onRetry={() => void release.refetch()}
            testId="release-stale-banner"
          />
        ) : null}
        <ReleaseDetails release={release.data} />
      </>
    );
  } else if (release.isError) {
    content = (
      <ErrorCallout
        error={release.error}
        startClass
        {...(loading
          ? {}
          : {
              onRetry: async () => {
                await release.refetch();
              },
            })}
      />
    );
  } else {
    content = <Spinner label="Loading release decision" />;
  }

  return (
    <div className="flex flex-col gap-4">
      <div className="flex items-center justify-between gap-3">
        <h2 className="text-base font-semibold text-fg">Release — {runId}</h2>
        <Tooltip content="Refresh">
          <Button
            size="icon"
            variant="ghost"
            aria-label="Refresh"
            disabled={loading}
            onClick={() => void release.refetch()}
          >
            <RefreshCw aria-hidden="true" />
          </Button>
        </Tooltip>
      </div>
      {content}
    </div>
  );
}
