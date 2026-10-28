import { CircleCheck, CircleX } from "lucide-react";

import { useApi } from "@/api/ApiProvider";
import type { RequestSummary } from "@/domain/request";
import { Button } from "@/ui/Button";
import { REQUEST_VERBS } from "@/domain/status";

import { isReviewState } from "./requestDetailLogic";
import { usePendingComments } from "@/shared/request/usePendingComments";
import type { RequestDialogs } from "./useRequestDialogs";

export interface ReviewActionsProps {
  readonly request: RequestSummary;
  readonly dialogs: RequestDialogs;
  /** GET /requests/{id} has itself returned, and nothing is refetching. */
  readonly detailLoaded: boolean;
  /** An editor is open: its text must be saved or cancelled before approving what it edits. */
  readonly anyEditorOpen: boolean;
}

/**
 * Approve and Request changes, offered only in the three review states
 * (the server refuses them from any other, `halted` included). Both are disabled, not hidden, without write access or while the
 * shown content is not the server's current.
 */
export function ReviewActions({
  request,
  dialogs,
  detailLoaded,
  anyEditorOpen,
}: ReviewActionsProps) {
  const { canWrite } = useApi();
  const pending = usePendingComments(request).comments.length;
  if (!isReviewState(request.state)) return null;
  const blocked = dialogs.acting || !canWrite || !detailLoaded || anyEditorOpen;
  return (
    <>
      <Button variant="primary" disabled={blocked} onClick={dialogs.openApprove}>
        <CircleCheck aria-hidden="true" />
        Approve
      </Button>
      <Button
        disabled={blocked}
        onClick={() => {
          dialogs.open("reject");
        }}
      >
        <CircleX aria-hidden="true" />
        {pending > 0
          ? `${REQUEST_VERBS.requestChanges} (${pending})`
          : REQUEST_VERBS.requestChanges}
      </Button>
    </>
  );
}
