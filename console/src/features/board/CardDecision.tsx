import { useEffect, useState } from "react";

import { useRequest } from "@/api/requestQueries";
import { compareTimestamps } from "@/domain/elapsed";
import type { RequestSummary } from "@/domain/request";
import { stateLabel } from "@/domain/status";
import { RejectDialog } from "@/shared/approval/RejectDialog";
import { Button } from "@/ui/Button";
import { ErrorCallout } from "@/ui/ErrorDisplay";
import { Callout, Spinner } from "@/ui/Feedback";

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
  // The stage the operator pressed Request changes on.
  const [pressedState] = useState(request.state);
  // A detail cached from an earlier visit can be older than the card (a
  // redraft since): it is read again, so a note is tied to a place in the
  // text as it is now.
  const [openedAt] = useState(() => Date.now());
  const stale = detail !== undefined && compareTimestamps(request.updatedAt, detail.updatedAt) > 0;
  useEffect(() => {
    if (stale) void refetch();
  }, [stale, refetch]);

  // The record the dialog is opened with, fixed once it is fresh (as Triage
  // fixes the record a decision is made on). The dialog is drawn from it
  // until it closes: a later refetch (the window regaining focus, an event
  // for this request) must not unmount it and lose what the operator typed.
  const [opened, setOpened] = useState<RequestSummary | null>(null);
  const fresh =
    detail !== undefined &&
    !query.isFetching &&
    query.error === null &&
    !(stale && query.dataUpdatedAt < openedAt);
  if (opened === null && fresh) setOpened(detail);

  if (opened !== null) {
    // The request left the stage the card showed while its record was read:
    // a rejection now would be of another stage's work, which nobody chose.
    if (opened.state !== pressedState) {
      return (
        <div className="relative z-10 flex flex-col items-start gap-1.5">
          <Callout tone="warning" data-testid="card-moved-on">
            {`This request has moved on to ${stateLabel(opened.state)}. Nothing was sent.`}
          </Callout>
          <Button size="sm" onClick={onClose}>
            Dismiss
          </Button>
        </div>
      );
    }
    return (
      <RejectDialog
        open
        request={opened}
        onOpenChange={(open) => {
          if (!open) onClose();
        }}
      />
    );
  }
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
  // Between the press and the dialog: the request is being read.
  return <Spinner label="Loading the request" className="relative z-10" />;
}
