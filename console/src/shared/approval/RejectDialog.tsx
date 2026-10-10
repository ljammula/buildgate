import { type ReactNode, useState } from "react";

import { useApi } from "@/api/ApiProvider";
import { useRejectRequest } from "@/api/requestQueries";
import type { RejectionAnchor, RequestSummary } from "@/domain/request";
import { anchorTargets } from "@/domain/reviewAnchors";
import { REQUEST_VERBS } from "@/domain/status";

import { AnchoredNotes } from "./AnchoredNotes";
import type { FlowProps } from "./flowTypes";
import { OperatorGate } from "./OperatorGate";
import { ReasonFlow } from "./ReasonFlow";

/**
 * "Request changes": asks for a reason, notes on specific sections or
 * criteria of the document under review, or both, then POSTs
 * `/requests/{id}/reject` with `{ reason, by, anchors }` (sends a spec_review
 * or plan_review request back to its drafting state). The places offered are
 * read from the request as shown. Renders nothing when
 * `open` is false or the console cannot write (`useApi().canWrite`): an
 * action the server would 403 is never offered. Prompts for the operator
 * name first when none is stored.
 */
export interface RejectDialogProps extends FlowProps {
  /**
   * Set when the request is no longer the one this dialog was opened on (it
   * moved to another stage, or was redrafted): the rejection would land on
   * work the operator has not seen, so it cannot be sent. The reason and the
   * notes typed so far stay visible.
   */
  readonly blocked?: ReactNode;
}

export function RejectDialog({ request, open, onOpenChange, onDone, blocked }: RejectDialogProps) {
  const { canWrite } = useApi();
  if (!open || !canWrite) return null;
  return (
    <OperatorGate onOpenChange={onOpenChange}>
      {(by) => (
        <RejectBody
          request={request}
          by={by}
          onOpenChange={onOpenChange}
          onDone={onDone}
          blocked={blocked}
        />
      )}
    </OperatorGate>
  );
}

interface BodyProps {
  readonly request: RequestSummary;
  readonly by: string;
  readonly onOpenChange: (open: boolean) => void;
  readonly onDone: FlowProps["onDone"];
  readonly blocked: ReactNode;
}

function RejectBody({ request, by, onOpenChange, onDone, blocked }: BodyProps) {
  const reject = useRejectRequest(request.id);
  // Fixed when the dialog opens, like an approval's hashes: a redraft
  // arriving underneath must not re-point a note already written.
  const [targets] = useState(() => anchorTargets(request));
  const [notes, setNotes] = useState<readonly RejectionAnchor[]>([]);
  // The stage on screen when the dialog opened is the one being rejected. It
  // is fixed here too: the caller's record can change underneath (a redraft
  // arriving), and sending the new stage would defeat the server's check
  // that the request is still where the operator saw it.
  const [seen] = useState({ state: request.state, enteredAt: request.enteredAt });
  return (
    <ReasonFlow
      title={REQUEST_VERBS.requestChanges}
      confirmLabel={REQUEST_VERBS.requestChanges}
      reasonOptional={notes.length > 0}
      blocked={blocked}
      pending={reject.isPending}
      error={reject.error}
      onOpenChange={onOpenChange}
      write={(reason) => reject.mutateAsync({ reason, by, anchors: notes, seen })}
      onDone={onDone}
    >
      <AnchoredNotes
        targets={targets}
        notes={notes}
        onChange={setNotes}
        disabled={reject.isPending}
      />
    </ReasonFlow>
  );
}
