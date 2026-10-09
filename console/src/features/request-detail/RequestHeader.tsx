import { RefreshCw } from "lucide-react";
import type { ReactNode } from "react";
import { Link } from "react-router";

import { requestStageGroupOf } from "@/domain/boardFilters";
import {
  type RequestSummary,
  activeJobLabel,
  requestAwaitingPullRequest,
  requestRunningJob,
  requestShortTitle,
} from "@/domain/request";
import { boardPath } from "@/routes/paths";
import { MemoryBadge } from "@/shared/request/MemoryBadge";
import { RequestStageChip } from "@/shared/request/RequestStageChip";
import { WaitingBadge } from "@/shared/request/WaitingBadge";
import { Badge } from "@/ui/Badge";
import { Button } from "@/ui/Button";
import { CompactId } from "@/ui/CompactId";
import { PageHeader } from "@/ui/PageLayout";
import { useNow } from "@/ui/Time";

export interface RequestHeaderProps {
  /** Null while loading or when the load failed: a bare "Request detail" header. */
  readonly request: RequestSummary | null;
  readonly refreshing: boolean;
  readonly onRefresh: () => void;
  /** The primary actions of the current state. */
  readonly actions?: ReactNode;
}

function RequestChips({ request }: { readonly request: RequestSummary }) {
  const now = useNow(60_000);
  const job = requestRunningJob(request);
  return (
    <span className="inline-flex flex-wrap items-center gap-2">
      <CompactId value={request.id} max={34} label="request id" className="text-xs" />
      <span>{request.project}</span>
      <MemoryBadge request={request} />
      <RequestStageChip
        state={request.state}
        awaitingPullRequest={requestAwaitingPullRequest(request)}
        waitingOn={request.waitingOn}
        needsYou={requestStageGroupOf(request) === "review"}
      />
      {job === null ? null : (
        <Badge data-testid="request-detail-active-job">{activeJobLabel(job)}</Badge>
      )}
      <WaitingBadge request={request} now={now} />
    </span>
  );
}

/**
 * The page header: the request's short title (the page's one h1), its id,
 * project, state chip, active job and waiting time, and on the right a
 * Refresh and the current state's primary actions. The global navigation is
 * the app shell's; this adds no second bar.
 */
export function RequestHeader({ request, refreshing, onRefresh, actions }: RequestHeaderProps) {
  return (
    <PageHeader
      sticky
      title={request === null ? "Request detail" : requestShortTitle(request)}
      breadcrumbs={<Link to={boardPath()}>Back to board</Link>}
      {...(request === null ? {} : { description: <RequestChips request={request} /> })}
      actions={
        <>
          <Button
            variant="ghost"
            size="icon"
            aria-label="Refresh"
            disabled={refreshing}
            onClick={onRefresh}
          >
            <RefreshCw aria-hidden="true" />
          </Button>
          {actions}
        </>
      }
    />
  );
}
