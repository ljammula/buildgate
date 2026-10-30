import { useState } from "react";

import { useApi } from "@/api/ApiProvider";
import { useApproveRequest } from "@/api/requestQueries";
import { formatUsageLines } from "@/domain/cost";
import { expectedSha256For } from "@/domain/contentHash";
import type { RequestSummary } from "@/domain/request";
import { stateLabel } from "@/domain/status";
import type { CostSummary } from "@/domain/usage";

import { nextStateAfterApprove } from "./approvalText";
import { FlowDialog } from "./FlowDialog";
import type { FlowProps } from "./flowTypes";
import { OperatorGate } from "./OperatorGate";

export interface ApproveDialogProps extends FlowProps {
  /**
   * Used for the "Usage so far" lines when `request` carries no
   * `costSummary` (the detail route never returns one; an earlier list
   * response for the same request can supply it). Null/absent shows "—".
   */
  readonly costSummary?: CostSummary | null;
  /**
   * The read-back questions whose answer box holds text not yet saved: the
   * dialog warns that the text is not part of this approval.
   */
  readonly unsavedAnswers?: readonly string[];
}

/**
 * "Approve this request?": restates the state transition, ticket count,
 * usage so far and prior rejections, then POSTs `/requests/{id}/approve`
 * with `{ by, expected_sha256 }`. The hashes bind the approval to the
 * content the operator was shown: they are computed from the `request` prop, never from a fetch made at confirm
 * time, so a spec or ticket changed on disk since the display makes the
 * server refuse (409) and the error stays on screen with the dialog open.
 * Renders nothing when closed or when the console cannot write; prompts
 * for the operator name first when none is stored.
 */
export function ApproveDialog({
  request,
  costSummary = null,
  unsavedAnswers = [],
  open,
  onOpenChange,
  onDone,
}: ApproveDialogProps) {
  const { canWrite } = useApi();
  if (!open || !canWrite) return null;
  return (
    <OperatorGate onOpenChange={onOpenChange}>
      {(by) => (
        <ApproveBody
          request={request}
          costSummary={costSummary}
          unsavedAnswers={unsavedAnswers}
          by={by}
          onOpenChange={onOpenChange}
          onDone={onDone}
        />
      )}
    </OperatorGate>
  );
}

interface BodyProps {
  readonly request: RequestSummary;
  readonly costSummary: CostSummary | null;
  readonly unsavedAnswers: readonly string[];
  readonly by: string;
  readonly onOpenChange: (open: boolean) => void;
  readonly onDone: FlowProps["onDone"];
}

function ApproveBody({
  request: latestRequest,
  costSummary,
  unsavedAnswers,
  by,
  onOpenChange,
  onDone,
}: BodyProps) {
  // The approval is bound to what was on screen when the operator pressed
  // Approve. The caller's record can change while this dialog is open (the
  // event stream delivers a redraft), and the operator has not read that
  // version: the record and hashes are fixed here, when the dialog opens, so
  // a change since then is refused by the server instead of being approved
  // unread.
  const [request] = useState(latestRequest);
  const approve = useApproveRequest(request.id);
  const next =
    request.approveNextState !== ""
      ? request.approveNextState
      : (nextStateAfterApprove(request.state) ?? request.state);
  const cost = request.costSummary ?? costSummary;

  async function confirm() {
    try {
      const updated = await approve.mutateAsync({
        by,
        expectedSha256: expectedSha256For(request),
      });
      onDone?.(updated);
      onOpenChange(false);
    } catch {
      // The mutation's error is shown by the dialog; stay open.
    }
  }

  return (
    <FlowDialog
      open
      onOpenChange={onOpenChange}
      title="Approve this request?"
      description={
        <ul className="flex flex-col gap-0.5">
          <li>
            {stateLabel(request.state)} → {stateLabel(next)}
          </li>
          <li>
            {request.ticketCount === 0 ? "Tickets: none yet" : `Tickets: ${request.ticketCount}`}
          </li>
          {cost === null ? (
            <li>Usage so far: —</li>
          ) : (
            formatUsageLines(cost).map((line) => <li key={line}>Usage so far: {line}</li>)
          )}
          <li>Prior rejections: {request.rejections.length}</li>
          {request.state === "spec_review" && unsavedAnswers.length > 0 ? (
            <li className="text-tone-danger mt-2">
              {`Unsaved answer${unsavedAnswers.length === 1 ? "" : "s"} to ${unsavedAnswers.join(", ")}: ` +
                "not part of this approval and not sent to the build. Save or clear it first, or approve without it."}
            </li>
          ) : null}
        </ul>
      }
      confirmLabel="Approve"
      dismissOnOutside
      pending={approve.isPending}
      error={approve.error}
      onConfirm={() => void confirm()}
    />
  );
}
