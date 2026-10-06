import type { ReactNode } from "react";

import { describeError } from "@/ui/ErrorDisplay";
import { formatUsageLines } from "@/domain/cost";
import type { RequestSummary } from "@/domain/request";
import { requestAwaitingPullRequest } from "@/domain/request";
import { EscapedText } from "@/shared/oracle/EscapedText";
import { LocalTimeText } from "@/ui/Time";
import { TicketRollupStrip } from "@/ui/TicketRollupStrip";

import { Panel } from "./Panel";

function Fact({ label, children }: { readonly label: string; readonly children: ReactNode }) {
  return (
    <div className="grid grid-cols-[7rem_minmax(0,1fr)] gap-2 text-sm">
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

/** The side column's facts: project, workspace, times, ticket progress, error, usage. */
export function RequestFacts({ request, refreshError }: RequestFactsProps) {
  const usage = request.costSummary === null ? [] : formatUsageLines(request.costSummary);
  return (
    <Panel title="Request">
      <dl className="flex flex-col gap-1.5">
        <Fact label="Project">{request.project}</Fact>
        <Fact label="Workspace">
          <span className="font-mono text-xs break-all">{request.workspace}</span>
        </Fact>
        <Fact label="Submitted">
          <LocalTimeText value={request.submittedAt} />
        </Fact>
        <Fact label="Updated">
          <LocalTimeText value={request.updatedAt} />
        </Fact>
        {request.ticketCount > 0 ? (
          <Fact label="Ticket">{`${request.ticketIndex} / ${request.ticketCount}`}</Fact>
        ) : null}
        {request.error === "" ? null : (
          // The "Next" banner carries the next step; this is only the detail.
          <Fact label={requestAwaitingPullRequest(request) ? "Detail" : "Error"}>
            <EscapedText text={request.error} />
          </Fact>
        )}
        {refreshError === null ? null : (
          <Fact label="Live updates">{`Refresh failed: ${describeError(refreshError).raw}`}</Fact>
        )}
        {usage.length === 0 ? null : (
          <Fact label="Usage">
            <ul className="font-mono text-xs">
              {usage.map((line) => (
                <li key={line}>{line}</li>
              ))}
            </ul>
          </Fact>
        )}
      </dl>
      {request.state === "building" || request.state === "pr_review" ? (
        <TicketRollupStrip request={request} />
      ) : null}
    </Panel>
  );
}
