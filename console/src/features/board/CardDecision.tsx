import { useEffect, useState } from "react";

import { useRequest } from "@/api/requestQueries";
import { compareTimestamps } from "@/domain/elapsed";
import type { RequestSummary } from "@/domain/request";
import { RejectDialog } from "@/shared/approval/RejectDialog";
import { Button } from "@/ui/Button";
import { ErrorCallout } from "@/ui/ErrorDisplay";

export interface CardDecisionProps {
  /** The board's record: no spec or ticket text, so the dialog waits for the detail. */
  readonly request: RequestSummary;
  readonly onClose: () => void;
}

/**
 * Request changes, started from a card: the shared dialog over the request's
 * own record, as Triage opens it. It is mounted only once the operator
 * presses the card's button, so no card fetches anything. The board list
 * carries no spec or ticket text, and the dialog offers the sections and
 * criteria a note can be tied to: those are read from `GET /requests/{id}`,
 * fetched after the press and never older than the card.
 *
 * A card never approves. An approval passes a human gate, and the text it
 * approves is on the request page, not on a card: the card links there.
 */
export function CardDecision({ request, onClose }: CardDecisionProps) {
  const query = useRequest(request.id);
  const { refetch } = query;
  const detail = query.data;
  // A detail cached from an earlier visit can be older than the card (a
  // redraft since): it is read again, so a note is tied to a place in the
  // text as it is now.
  const [openedAt] = useState(() => Date.now());
  const stale = detail !== undefined && compareTimestamps(request.updatedAt, detail.updatedAt) > 0;
  useEffect(() => {
    if (stale) void refetch();
  }, [stale, refetch]);

  if (query.error !== null) {
    return (
      <div className="relative z-10 flex flex-col items-start gap-1.5">
        <ErrorCallout error={query.error} />
        <Button size="sm" onClick={onClose}>
          Dismiss
        </Button>
      </div>
    );
  }
  if (detail === undefined || query.isFetching) return null;
  if (stale && query.dataUpdatedAt < openedAt) return null;
  const close = (open: boolean): void => {
    if (!open) onClose();
  };
  return <RejectDialog open request={detail} onOpenChange={close} />;
}
