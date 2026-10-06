import { Check, X } from "lucide-react";

import { formatUsageLines } from "@/domain/cost";
import type { RequestSummary } from "@/domain/request";
import { RequestStageChip } from "@/shared/request/RequestStageChip";
import { WaitingBadge } from "@/shared/request/WaitingBadge";
import { Button } from "@/ui/Button";
import { CodeBlock } from "@/ui/CodeBlock";
import { Spinner } from "@/ui/Feedback";
import { ErrorCallout } from "@/ui/ErrorDisplay";
import { TicketRollupStrip } from "@/ui/TicketRollupStrip";

import { artifactContent } from "./triageModel";

export interface TriageDetailProps {
  /** The board's record: metadata, usage and prior rejections. */
  readonly request: RequestSummary;
  /** The fetched `GET /requests/{id}` for that request; null until it loads. */
  readonly detail: RequestSummary | null;
  readonly detailError: unknown;
  readonly canWrite: boolean;
  /** A decision dialog is open. */
  readonly acting: boolean;
  readonly now: Date;
  readonly onApprove: () => void;
  readonly onReject: () => void;
}

/**
 * The focused request: its metadata and the spec or plan under review. The
 * board list never carries file content, so approve and reject stay disabled
 * until the detail has loaded: the operator always sees what they decide on.
 */
export function TriageDetail({
  request,
  detail,
  detailError,
  canWrite,
  acting,
  now,
  onApprove,
  onReject,
}: TriageDetailProps) {
  const cost = request.costSummary;
  const content = detail === null ? "" : artifactContent(detail);
  return (
    <div className="border-border flex min-w-0 flex-col gap-3 rounded-lg border p-4">
      <h2 className="text-fg text-base font-semibold">
        {request.title !== "" ? request.title : request.id}
      </h2>
      <div className="flex flex-wrap items-center gap-2">
        <RequestStageChip state={request.state} needsYou />
        <WaitingBadge request={request} now={now} />
      </div>
      <div className="text-fg-muted flex flex-col gap-0.5 text-sm">
        <span>{`Project: ${request.project}`}</span>
        {request.ticketCount > 0 ? (
          <span>{`Ticket ${request.ticketIndex} / ${request.ticketCount}`}</span>
        ) : null}
        {cost === null ? (
          <span>Usage so far: —</span>
        ) : (
          formatUsageLines(cost).map((line) => <span key={line}>{`Usage so far: ${line}`}</span>)
        )}
        {request.rejections.length > 0 ? (
          <span>{`Prior rejections: ${request.rejections.length}`}</span>
        ) : null}
      </div>
      {request.tickets.length > 0 ? <TicketRollupStrip request={request} /> : null}
      <h3 className="text-fg text-sm font-semibold">Under review:</h3>
      {detail !== null ? (
        <div data-testid="triage-artifact-content">
          <CodeBlock wrap maxHeight="max-h-[55vh]" label="Under review">
            {content === "" ? "(no content yet)" : content}
          </CodeBlock>
        </div>
      ) : detailError !== null ? (
        <div data-testid="triage-detail-error">
          <ErrorCallout error={detailError} />
        </div>
      ) : (
        <Spinner />
      )}
      {/* Offered only when the console can write: a refused write would just
          show the server's 403, so an action doomed to fail is not offered. */}
      <div
        data-testid="triage-decision-bar"
        className="border-border bg-surface sticky bottom-0 z-10 -mx-4 -mb-4 flex flex-col gap-2 rounded-b-lg border-t px-4 py-3"
      >
        {canWrite ? (
          <div className="flex flex-wrap gap-2">
            <Button variant="primary" disabled={acting || detail === null} onClick={onApprove}>
              <Check aria-hidden="true" />
              Approve (a)
            </Button>
            <Button disabled={acting || detail === null} onClick={onReject}>
              <X aria-hidden="true" />
              Reject (r)
            </Button>
          </div>
        ) : null}
        <p className="text-fg-subtle text-xs">Keyboard: j/k move · a approve · r reject</p>
      </div>
    </div>
  );
}
