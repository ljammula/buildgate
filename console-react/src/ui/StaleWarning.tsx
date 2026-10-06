import type { ReactNode } from "react";

import { Button } from "@/ui/Button";
import { describeError, ErrorCallout } from "@/ui/ErrorDisplay";
import { Callout } from "@/ui/Feedback";

/** How much of the failure the warning writes out; see `StaleWarningProps.detail`. */
type StaleDetail = "raw" | "headline" | "next-step" | "callout";

export interface StaleWarningProps {
  /** Why the refresh failed. */
  readonly error: unknown;
  /**
   * What the screen says the failure means. Default: "Showing the last
   * successfully loaded data -- refresh failed:". With `detail="callout"` it
   * is the note under the error (what is stale and what is disabled).
   */
  readonly children?: ReactNode;
  /**
   * How the error follows the message: "raw" (default) the error's own text,
   * "headline" its short classification, "next-step" the operator's next
   * step, "callout" the full `ErrorCallout` (headline, next step, Details)
   * stacked above the message, with no warning box around it.
   */
  readonly detail?: StaleDetail;
  /** The error came from a start-token-gated call, so a 401/403 names that fix. */
  readonly startClass?: boolean;
  /** Shows a Retry button beside the message. */
  readonly onRetry?: () => void;
  /** A refresh is in flight: Retry is disabled, so a stale response cannot land after a newer one. */
  readonly retrying?: boolean;
  readonly testId?: string;
  readonly className?: string;
}

const defaultMessage = "Showing the last successfully loaded data -- refresh failed:";

/** A failed refresh next to the last successfully loaded data, which stays on screen. */
export function StaleWarning({
  error,
  children = defaultMessage,
  detail = "raw",
  startClass = false,
  onRetry,
  retrying = false,
  testId,
  className,
}: StaleWarningProps) {
  const testIdProp = testId === undefined ? {} : { "data-testid": testId };
  if (detail === "callout") {
    return (
      <div {...testIdProp} className={className ?? "flex flex-col gap-1"}>
        <ErrorCallout error={error} startClass={startClass} />
        <p className="text-sm">{children}</p>
      </div>
    );
  }
  const summary = describeError(error, { startClass });
  const text =
    detail === "headline"
      ? summary.headline
      : detail === "next-step"
        ? summary.nextStep
        : summary.raw;
  return (
    <Callout tone="warning" className={className} {...testIdProp}>
      <div className="flex items-center justify-between gap-3">
        <span>
          {children} {text}
        </span>
        {onRetry === undefined ? null : (
          <Button size="sm" disabled={retrying} onClick={onRetry}>
            Retry
          </Button>
        )}
      </div>
    </Callout>
  );
}
