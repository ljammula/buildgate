import { useId } from "react";

import { useDaemons, useQueueRunStatus } from "@/api/runQueries";
import { workerLiveness } from "@/domain/ops";
import { Badge } from "@/ui/Badge";
import { Card, CardBody, CardHeader, CardTitle } from "@/ui/Card";
import { CopyableCommand } from "@/ui/CopyableCommand";
import { ErrorCallout } from "@/ui/ErrorDisplay";
import { Spinner } from "@/ui/Feedback";
import { RelativeTime } from "@/ui/RelativeTime";

/**
 * Whether a worker is draining this data directory, from GET /queue-run (the
 * read route that answers however the worker was started), and the
 * supervisors `factoryd serve` manages itself when GET /daemons lists any.
 * That second route answers 403 or 404 on a server that manages none: then
 * nothing is shown for it. Observability only.
 */
export function DaemonStatusCard() {
  const titleId = useId();
  const queueRun = useQueueRunStatus();
  const daemons = useDaemons();
  const managed = daemons.data ?? [];

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
      <CardBody className="flex flex-col gap-3">
        {worker}
        {managed.length === 0 ? null : (
          <ul aria-label="Started by serve" className="flex flex-col gap-1 text-sm">
            {managed.map((daemon) => (
              <li key={daemon.repository} className="flex flex-wrap items-center gap-2">
                <span className="font-mono text-xs break-all">{daemon.repository}</span>
                <span className="text-fg-muted text-xs">{daemon.state}</span>
                {daemon.startedAt === "" ? null : (
                  <span className="text-fg-muted text-xs">
                    started <RelativeTime value={daemon.startedAt} />
                  </span>
                )}
              </li>
            ))}
          </ul>
        )}
      </CardBody>
    </Card>
  );
}
