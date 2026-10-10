import { type ReactNode, useEffect, useState } from "react";

import { useRequest } from "@/api/requestQueries";
import { compareTimestamps } from "@/domain/elapsed";
import { type RequestSummary, requestShortTitle } from "@/domain/request";
import { stateLabel } from "@/domain/status";
import { movedOnNotice } from "@/shared/approval/movedOn";
import { RejectDialog } from "@/shared/approval/RejectDialog";
import { Button } from "@/ui/Button";
import { ErrorCallout } from "@/ui/ErrorDisplay";
import { Callout, Spinner } from "@/ui/Feedback";

export interface CardDecisionProps {
  /**
   * The request as the list has it NOW (it changes under an open dialog): no
   * spec or ticket text, so the dialog waits for the detail, and the record
   * every later change is checked against.
   */
  readonly request: RequestSummary;
  readonly onClose: () => void;
}

/**
 * Request changes, started from a card: the shared dialog over the request's
 * own record, as Triage opens it. The screen mounts it once, outside the
 * board's lanes and columns, when the operator presses a card's button: no
 * card fetches anything, and no change to the list (a lane appearing, a card
 * changing column) can unmount it and lose what was typed. The board list
 * carries no spec or ticket text, and the dialog offers the sections and
 * criteria a note can be tied to: those are read from `GET /requests/{id}`,
 * fetched after the press and never older than the card.
 *
 * The rejection names the stage it is of (`expected_state`,
 * `expected_entered_at`, from the record the dialog was opened on) and the
 * server refuses it (409) once the request has left that stage or entered it
 * again, so it can never land on work the operator did not see, however old
 * the list is. The dialog still compares the list's record with the one it
 * was opened on and stops sending once they differ: that says so before the
 * operator presses send, with what was typed left on screen to be copied.
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

  const title = requestShortTitle(request);
  if (opened !== null) {
    // The request left the stage the card showed while its record was read:
    // a rejection now would be of another stage's work, which nobody chose.
    if (opened.state !== pressedState) {
      return (
        <DecisionStatus title={title} onClose={onClose}>
          <Callout tone="warning" data-testid="card-moved-on">
            {`This request has moved on to ${stateLabel(opened.state)}. Nothing was sent.`}
          </Callout>
        </DecisionStatus>
      );
    }
    return (
      <RejectDialog
        open
        request={opened}
        blocked={movedOnNotice(request, opened)}
        onOpenChange={(open) => {
          if (!open) onClose();
        }}
      />
    );
  }
  if (query.error !== null) {
    return (
      <DecisionStatus title={title} onClose={onClose}>
        <ErrorCallout error={query.error} />
      </DecisionStatus>
    );
  }
  // Between the press and the dialog: the request is being read.
  return (
    <div data-testid="card-decision" className="flex items-center gap-2 text-sm">
      <Spinner label="Loading the request" />
      <span className="text-fg-muted">{`Request changes: reading ${title}`}</span>
    </div>
  );
}

// What stands in for the dialog when it cannot open: said above the board,
// naming the request, with the way to put it away.
function DecisionStatus({
  title,
  onClose,
  children,
}: {
  readonly title: string;
  readonly onClose: () => void;
  readonly children: ReactNode;
}) {
  return (
    <div data-testid="card-decision" className="flex flex-col items-start gap-1.5">
      <p className="text-fg-muted text-sm">{`Request changes: ${title}`}</p>
      {children}
      <Button size="sm" onClick={onClose}>
        Dismiss
      </Button>
    </div>
  );
}
