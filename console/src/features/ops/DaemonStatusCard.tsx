import { useId } from "react";

import { useQueueRunStatus } from "@/api/runQueries";
import { workerLiveness } from "@/domain/ops";
import { Badge } from "@/ui/Badge";
import { Card, CardBody, CardHeader, CardTitle } from "@/ui/Card";
import { CopyableCommand } from "@/ui/CopyableCommand";
import { ErrorCallout } from "@/ui/ErrorDisplay";
import { Spinner } from "@/ui/Feedback";
import { RelativeTime } from "@/ui/RelativeTime";

/**
 * Whether a worker is draining this data directory, from GET /queue-run: the
 * read route that answers however the worker was started. The supervisors
 * `factoryd serve` can manage itself are not listed: their route answers 404
 * on the usual server, which manages none, and a page that asks anyway fails
 * a request on every visit. Observability only.
 */
export function DaemonStatusCard() {
  const titleId = useId();
  const queueRun = useQueueRunStatus();

  let worker;
  if (queueRun.data !== undefined) {
    const liveness = workerLiveness(queueRun.data.state);
    worker = (
      <>
        <div className="flex flex-wrap items-center gap-3 text-sm">
          <span className="font-medium">Worker</span>
          <Badge tone={liveness.tone}>{liveness.label}</Badge>
          {queueRun.data.lastHeartbeat === "" ? null : (
            <span className="text-fg-muted text-xs">
              last heartbeat <RelativeTime value={queueRun.data.lastHeartbeat} />
            </span>
          )}
        </div>
        {liveness.alive ? null : (
          <CopyableCommand command="factoryd worker" label="Start the worker:" />
        )}
      </>
    );
  } else if (queueRun.isError) {
    worker = <ErrorCallout error={queueRun.error} onRetry={() => void queueRun.refetch()} />;
  } else {
    worker = <Spinner label="Loading the worker's status" />;
  }

  return (
    <Card role="region" aria-labelledby={titleId}>
      <CardHeader className="py-2.5">
        <CardTitle id={titleId} className="text-sm">
          Daemons
        </CardTitle>
      </CardHeader>
      <CardBody className="flex flex-col gap-3">{worker}</CardBody>
    </Card>
  );
}
