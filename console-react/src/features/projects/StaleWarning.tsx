import { Button } from "@/ui/Button";
import { Callout } from "@/ui/Feedback";
import { describeError } from "@/ui/ErrorDisplay";

export interface StaleWarningProps {
  readonly error: unknown;
  /** True while a refresh is in flight: Retry is disabled so a stale response cannot land after a newer one. */
  readonly fetching: boolean;
  readonly onRetry: () => void;
}

/** A failed refresh next to the last successfully loaded data, which stays on screen. */
export function StaleWarning({ error, fetching, onRetry }: StaleWarningProps) {
  return (
    <Callout tone="warning">
      <div className="flex items-center justify-between gap-3">
        <span>
          Showing the last successfully loaded data -- refresh failed: {describeError(error).raw}
        </span>
        <Button size="sm" disabled={fetching} onClick={onRetry}>
          Retry
        </Button>
      </div>
    </Callout>
  );
}
