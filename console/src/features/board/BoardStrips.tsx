import type { ApiError } from "@/domain/apiError";
import { Button } from "@/ui/Button";
import { Callout } from "@/ui/Feedback";
import { describeError } from "@/ui/ErrorDisplay";
import { StaleWarning } from "@/ui/StaleWarning";

export interface BoardStripsProps {
  /** Config's release-policy warning: the policy denies every PR. */
  readonly releasePolicyWarning: string | null;
  /** queueRunWarning's text, or null to hide the worker strip. */
  readonly workerWarning: string | null;
  readonly onRetryWorker: () => void;
  /** A permanent event-stream failure: the board then refreshes by polling only. */
  readonly streamError: ApiError | null;
  /**
   * How many requests wait on the operator, and whether the board in view
   * already lists them in its Needs you section. The strip appears only when
   * a filter hides that section: otherwise the section is the notice.
   */
  readonly needsYouCount: number;
  readonly needsYouVisible: boolean;
  readonly onViewNeedsYou: () => void;
  /** A failed refresh while data from an earlier load is on screen. */
  readonly refreshError: unknown;
  readonly refreshing: boolean;
  readonly onRetryRefresh: () => void;
}

/** The warnings above the board: each one explains a silence the list alone would hide. */
export function BoardStrips(props: BoardStripsProps) {
  return (
    <>
      {/* The server's release policy can never allow a release decision:
          without this an operator only learns it after an accepted run
          produces no PR. */}
      {props.releasePolicyWarning === null ? null : (
        <Callout tone="warning" data-testid="release-policy-warning-banner">
          Release policy denies every PR: {props.releasePolicyWarning}
        </Callout>
      )}
      {/* The worker advances a request between polls: without this, a
          request stuck because it is down reads like one making progress. */}
      {props.workerWarning === null ? null : (
        <Callout tone="danger" data-testid="worker-down-banner">
          <div className="flex items-center justify-between gap-3">
            <span>{props.workerWarning}</span>
            <Button size="sm" onClick={props.onRetryWorker}>
              Retry
            </Button>
          </div>
        </Callout>
      )}
      {props.streamError === null ? null : (
        <Callout tone="warning" data-testid="board-stream-banner">
          Live updates stopped: {describeError(props.streamError).headline}. The board still
          refreshes every 5 seconds.
        </Callout>
      )}
      {props.needsYouCount > 0 && !props.needsYouVisible ? (
        <Callout tone="warning" data-testid="needs-you-banner">
          <div className="flex items-center justify-between gap-3">
            <span>
              {props.needsYouCount === 1
                ? "1 request is waiting for your review"
                : `${props.needsYouCount} requests are waiting for your review`}
            </span>
            <Button size="sm" onClick={props.onViewNeedsYou}>
              View
            </Button>
          </div>
        </Callout>
      ) : null}
      {props.refreshError === null ? null : (
        <StaleWarning
          error={props.refreshError}
          detail="headline"
          retrying={props.refreshing}
          onRetry={props.onRetryRefresh}
          testId="request-list-stale-banner"
        />
      )}
    </>
  );
}
