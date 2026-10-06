import type { ReactNode } from "react";

import { waitingBadgeLabel } from "@/domain/requestOrder";
import type { RequestSummary } from "@/domain/request";
import { WaitingBadge } from "@/shared/request/WaitingBadge";

export interface RequestStatusUnitProps {
  readonly request: RequestSummary;
  readonly now: Date;
  /** The request's state chip (RequestStageChip), drawn by the caller. */
  readonly children: ReactNode;
}

/**
 * The state chip and the "Waiting on you · 5m" badge as one pill: two chips
 * side by side read as two statuses competing, and they are one fact. A
 * request that does not wait on the operator is its state chip alone.
 */
export function RequestStatusUnit({ request, now, children }: RequestStatusUnitProps) {
  if (waitingBadgeLabel(request, now) === null) return <>{children}</>;
  return (
    <span
      data-testid="request-status-unit"
      className="inline-flex items-center [&>:first-child]:rounded-r-none [&>:first-child]:border-r-0 [&>:last-child]:rounded-l-none"
    >
      {children}
      <WaitingBadge request={request} now={now} />
    </span>
  );
}
