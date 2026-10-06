import { useState } from "react";

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
export function RejectDialog({ request, open, onOpenChange, onDone }: FlowProps) {
  const { canWrite } = useApi();
  if (!open || !canWrite) return null;
  return (
    <OperatorGate onOpenChange={onOpenChange}>
      {(by) => <RejectBody request={request} by={by} onOpenChange={onOpenChange} onDone={onDone} />}
    </OperatorGate>
  );
}

interface BodyProps {
  readonly request: RequestSummary;
  readonly by: string;
  readonly onOpenChange: (open: boolean) => void;
  readonly onDone: FlowProps["onDone"];
}

function RejectBody({ request, by, onOpenChange, onDone }: BodyProps) {
  const reject = useRejectRequest(request.id);
  // Fixed when the dialog opens, like an approval's hashes: a redraft
  // arriving underneath must not re-point a note already written.
  const [targets] = useState(() => anchorTargets(request));
  const [notes, setNotes] = useState<readonly RejectionAnchor[]>([]);
  return (
    <ReasonFlow
      title={REQUEST_VERBS.requestChanges}
      confirmLabel={REQUEST_VERBS.requestChanges}
      reasonOptional={notes.length > 0}
      pending={reject.isPending}
      error={reject.error}
      onOpenChange={onOpenChange}
      write={(reason) => reject.mutateAsync({ reason, by, anchors: notes })}
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
