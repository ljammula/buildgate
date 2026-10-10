import type { ReactNode } from "react";
import { Link } from "react-router";

import type { FactoryHealth } from "@/domain/health";
import { requestPath } from "@/routes/paths";
import { cn } from "@/ui/cn";
import { RelativeTime } from "@/ui/RelativeTime";
import { StallChip } from "@/ui/Time";
import { toneClasses } from "@/ui/tone";

export interface HealthStripProps {
  readonly health: FactoryHealth;
}

function Fact({ label, children }: { readonly label: string; readonly children: ReactNode }) {
  return (
    <div className="flex min-w-0 flex-wrap items-center gap-x-1.5 gap-y-1">
      <dt className="text-fg-muted">{label}</dt>
      <dd className="text-fg flex min-w-0 flex-wrap items-center gap-x-1.5 gap-y-1">{children}</dd>
    </div>
  );
}

/**
 * The line at the top of Mission Control: is a worker alive, how many of its
 * job slots are busy, what it runs right now, how many requests wait for a
 * slot, and when anything last moved. Every fact is the server's
 * (`GET /queue-run`, the request list); a fact it did not send is left out.
 */
export function HealthStrip({ health }: HealthStripProps) {
  const { worker, slots } = health;
  return (
    <section
      aria-label="Factory health"
      className="border-border bg-surface rounded-lg border px-4 py-2.5 text-sm"
    >
      <dl className="flex flex-wrap items-center gap-x-6 gap-y-2">
        {worker === null ? null : (
          <Fact label="Worker">
            <span className="inline-flex items-center gap-1.5 font-medium">
              <span
                aria-hidden
                className={cn("size-2 rounded-full bg-current", toneClasses[worker.tone].text)}
              />
              {worker.label}
            </span>
            {!worker.alive && health.lastHeartbeat !== "" ? (
              <span className="text-fg-muted">
                (last heartbeat <RelativeTime value={health.lastHeartbeat} />)
              </span>
            ) : null}
          </Fact>
        )}
        {slots === null ? null : (
          <Fact label="Job slots">{`${slots.busy} of ${slots.total} busy`}</Fact>
        )}
        {worker?.alive === true ? (
          <Fact label="Running">
            {health.running.length === 0 ? (
              <span className="text-fg-muted">nothing</span>
            ) : (
              <ul aria-label="Running now" className="flex flex-wrap items-center gap-x-4 gap-y-1">
                {health.running.map((job) => (
                  <li key={job.requestId} className="flex flex-wrap items-center gap-x-1.5">
                    <Link
                      to={requestPath(job.requestId)}
                      className="text-fg max-w-64 truncate font-medium hover:underline"
                      title={job.title}
                    >
                      {job.title}
                    </Link>
                    {job.detail === "" ? null : <span className="text-fg-muted">{job.detail}</span>}
                    {job.stalled ? (
                      <StallChip run={{ stalled: true, waitingReason: null }} />
                    ) : null}
                  </li>
                ))}
              </ul>
            )}
          </Fact>
        ) : null}
        <Fact label="Queued">{String(health.queued)}</Fact>
        {health.lastTransitionAt === null ? null : (
          <Fact label="Last transition">
            <RelativeTime value={health.lastTransitionAt} />
          </Fact>
        )}
      </dl>
    </section>
  );
}
