import { describeError } from "@/ui/ErrorDisplay";
import { formatUsageFigure, formatUsageLines } from "@/domain/cost";
import type { RequestSummary } from "@/domain/request";
import { requestAwaitingPullRequest } from "@/domain/request";
import { DigestedText } from "@/shared/request/DigestedText";
import { DescriptionItem, DescriptionList } from "@/ui/DescriptionList";
import { Disclosure } from "@/ui/Disclosure";
import { RelativeTime } from "@/ui/RelativeTime";
import { ShortPath } from "@/ui/ShortPath";
import { TicketRollupStrip } from "@/ui/TicketRollupStrip";

import { Panel } from "./Panel";

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
      <DescriptionList labelWidth="sm">
        <DescriptionItem label="Workspace">
          <ShortPath
            path={request.workspace}
            copyLabel="workspace path"
            className="break-words whitespace-normal"
          />
        </DescriptionItem>
        <DescriptionItem label="Updated">
          <RelativeTime value={request.updatedAt} />
        </DescriptionItem>
        {request.ticketCount > 0 ? (
          <DescriptionItem label="Tickets">{`${request.ticketIndex} / ${request.ticketCount}`}</DescriptionItem>
        ) : null}
        {/* A halted or quarantined request shows its cause in the recovery
            callout, beside the actions. */}
        {request.error === "" ||
        request.state === "halted" ||
        request.state === "quarantined" ? null : (
          // The "Next" banner carries the next step; this is only the detail.
          <DescriptionItem label={requestAwaitingPullRequest(request) ? "Detail" : "Error"}>
            <DigestedText text={request.error} fullLabel="Full text" />
          </DescriptionItem>
        )}
        {refreshError === null ? null : (
          <DescriptionItem label="Live updates">{`Refresh failed: ${describeError(refreshError).raw}`}</DescriptionItem>
        )}
        {summary === null ? null : (
          <DescriptionItem label="Usage">
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
          </DescriptionItem>
        )}
      </DescriptionList>
      {request.state === "building" || request.state === "pr_review" ? (
        <TicketRollupStrip request={request} />
      ) : null}
    </Panel>
  );
}
