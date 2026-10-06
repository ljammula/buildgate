import { RefreshCw } from "lucide-react";
import type { ReactNode } from "react";
import { Link } from "react-router";

import {
  type RequestSummary,
  activeJobLabel,
  requestAwaitingPullRequest,
  requestRunningJob,
} from "@/domain/request";
import { boardPath } from "@/routes/paths";
import { Badge } from "@/ui/Badge";
import { Button } from "@/ui/Button";
import { PageHeader } from "@/ui/PageLayout";
import { useNow } from "@/ui/Time";
import { requestShortTitle } from "@/domain/request";

import { RequestStateChip } from "./RequestStateChip";
import { requestNeedsYou, waitingBadge } from "./requestDetailLogic";

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
  const badge = waitingBadge(request, now);
  return (
    <span className="inline-flex flex-wrap items-center gap-2">
      <span className="font-mono text-xs">{request.id}</span>
      <span>{request.project}</span>
      <RequestStateChip
        state={request.state}
        awaitingPullRequest={requestAwaitingPullRequest(request)}
        waitingOn={request.waitingOn}
        needsYou={requestNeedsYou(request)}
      />
      {job === null ? null : (
        <Badge data-testid="request-detail-active-job">{activeJobLabel(job)}</Badge>
      )}
      {badge === null ? null : (
        <Badge tone="warning" data-testid="request-detail-waiting-badge">
          {badge}
        </Badge>
      )}
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
