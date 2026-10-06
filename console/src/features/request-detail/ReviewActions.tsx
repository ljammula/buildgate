import { CircleCheck, CircleX } from "lucide-react";

import { useApi } from "@/api/ApiProvider";
import type { RequestSummary } from "@/domain/request";
import { Button } from "@/ui/Button";
import { REQUEST_VERBS } from "@/domain/status";

import { isReviewState } from "./requestDetailLogic";
import { type RequestDialogs, awaitsTicketOracle } from "./useRequestDialogs";

/** The id of the element wrapping the plan_review ticket oracle panel. */
export const ticketOracleAnchorId = "ticket-oracle-anchor";

export interface ReviewActionsProps {
  readonly request: RequestSummary;
  readonly dialogs: RequestDialogs;
  /** GET /requests/{id} has itself returned, and nothing is refetching. */
  readonly detailLoaded: boolean;
  /** An editor is open: its text must be saved or cancelled before approving what it edits. */
  readonly anyEditorOpen: boolean;
}

/** The reason beside a disabled plan Approve: how many files are left, when that is known. */
export function oracleHint(remaining: number | null): string {
  if (remaining === null || remaining < 1) return "Open every oracle file first";
  return `Open ${remaining} oracle ${remaining === 1 ? "file" : "files"} first`;
}

/**
 * Approve and Request changes, offered only in the three review states
 * (the server refuses them from any other, `halted` included). Approve is
 * left to the oracle panel at oracle_review, which owns the shown-files
 * hashes. Both are disabled, not hidden, without write access or while the
 * shown content is not the server's current.
 */
export function ReviewActions({
  request,
  dialogs,
  detailLoaded,
  anyEditorOpen,
}: ReviewActionsProps) {
  const { canWrite } = useApi();
  if (!isReviewState(request.state)) return null;
  const blocked = dialogs.acting || !canWrite || !detailLoaded || anyEditorOpen;
  const ticketOracleIncomplete =
    awaitsTicketOracle(request) && dialogs.ticketOracle?.complete !== true;
  return (
    <>
      {ticketOracleIncomplete ? (
        // The reason is in the oracle panel far below; say it beside the button.
        <button
          type="button"
          data-testid="approve-needs-oracle"
          className="text-accent text-xs underline underline-offset-2"
          onClick={() => {
            document
              .getElementById(ticketOracleAnchorId)
              ?.scrollIntoView({ behavior: "smooth", block: "start" });
          }}
        >
          {oracleHint(dialogs.ticketOracle?.remaining ?? null)}
        </button>
      ) : null}
      {request.state === "oracle_review" ? null : (
        <Button
          variant="primary"
          disabled={blocked || ticketOracleIncomplete}
          onClick={dialogs.openApprove}
        >
          <CircleCheck aria-hidden="true" />
          Approve
        </Button>
      )}
      <Button
        disabled={blocked}
        onClick={() => {
          dialogs.open("reject");
        }}
      >
        <CircleX aria-hidden="true" />
        {REQUEST_VERBS.requestChanges}
      </Button>
    </>
  );
}
