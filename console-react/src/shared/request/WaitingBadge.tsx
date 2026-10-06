import type { RequestSummary } from "@/domain/request";
import { waitingBadgeLabel } from "@/domain/requestOrder";
import { cn } from "@/ui/cn";
import { toneClasses } from "@/ui/tone";

export interface WaitingBadgeProps {
  readonly request: RequestSummary;
  readonly now: Date;
}

/**
 * The "Waiting on you · 45m" pill, shown by the board, triage and the request
 * page. Renders nothing for a request that does not wait on the operator, so
 * a caller never checks the state first.
 */
export function WaitingBadge({ request, now }: WaitingBadgeProps) {
  const label = waitingBadgeLabel(request, now);
  if (label === null) return null;
  return (
    <span
      data-testid="waiting-badge"
      className={cn(
        "rounded-full border px-2 py-0.5 text-xs font-medium",
        toneClasses.warning.text,
        toneClasses.warning.border,
        toneClasses.warning.soft,
      )}
    >
      {label}
    </span>
  );
}
