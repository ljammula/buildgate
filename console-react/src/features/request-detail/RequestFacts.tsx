import type { ReactNode } from "react";

import { describeError } from "@/ui/ErrorDisplay";
import { formatUsageFigure, formatUsageLines } from "@/domain/cost";
import type { RequestSummary } from "@/domain/request";
import { requestAwaitingPullRequest } from "@/domain/request";
import { DigestedText } from "@/shared/request/DigestedText";
import { CompactId } from "@/ui/CompactId";
import { Disclosure } from "@/ui/Disclosure";
import { RelativeTime } from "@/ui/RelativeTime";
import { TicketRollupStrip } from "@/ui/TicketRollupStrip";

import { Panel } from "./Panel";

function Fact({ label, children }: { readonly label: string; readonly children: ReactNode }) {
  return (
    <div className="grid grid-cols-[5.5rem_minmax(0,1fr)] gap-2 text-sm">
      <dt className="text-xs leading-5 text-fg-muted">{label}</dt>
      <dd className="min-w-0 break-words">{children}</dd>
    </div>
  );
}

export interface RequestFactsProps {
  readonly request: RequestSummary;
  /** Set when the last refresh failed while an older record is still shown. */
  readonly refreshError: unknown;
}

/**
 * The side column's digest: where the work is (workspace, ticket n of m), how
 * long ago it last moved, one usage figure, and an error only when it is not
 * already in the recovery callout. The project is in the header; the
 * per-model breakdown is one disclosure away.
 */
export function RequestFacts({ request, refreshError }: RequestFactsProps) {
  const summary = request.costSummary;
  const breakdown = summary === null ? [] : formatUsageLines(summary);
  return (
    <Panel title="Request">
      <dl className="flex flex-col gap-1.5">
        <Fact label="Workspace">
          <CompactId
            value={request.workspace}
            max={22}
            label="workspace path"
            className="text-xs"
          />
        </Fact>
        <Fact label="Updated">
          <RelativeTime value={request.updatedAt} />
        </Fact>
        {request.ticketCount > 0 ? (
          <Fact label="Ticket">{`${request.ticketIndex} / ${request.ticketCount}`}</Fact>
        ) : null}
        {/* A halted or quarantined request shows its cause in the recovery
            callout, beside the actions. */}
        {request.error === "" ||
        request.state === "halted" ||
        request.state === "quarantined" ? null : (
          // The "Next" banner carries the next step; this is only the detail.
          <Fact label={requestAwaitingPullRequest(request) ? "Detail" : "Error"}>
            <DigestedText text={request.error} fullLabel="Full text" />
          </Fact>
        )}
        {refreshError === null ? null : (
          <Fact label="Live updates">{`Refresh failed: ${describeError(refreshError).raw}`}</Fact>
        )}
        {summary === null ? null : (
          <Fact label="Usage">
            <span>{formatUsageFigure(summary)}</span>
            {breakdown.length > 0 ? (
              <Disclosure bare title="By role and model" headingLevel="h3">
                <ul className="font-mono text-xs">
                  {breakdown.map((line) => (
                    <li key={line}>{line}</li>
                  ))}
                </ul>
              </Disclosure>
            ) : null}
          </Fact>
        )}
      </dl>
      {request.state === "building" || request.state === "pr_review" ? (
        <TicketRollupStrip request={request} />
      ) : null}
    </Panel>
  );
}
