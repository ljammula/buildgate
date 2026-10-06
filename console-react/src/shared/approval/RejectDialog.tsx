import { useApi } from "@/api/ApiProvider";
import { useRejectRequest } from "@/api/requestQueries";

import type { FlowProps } from "./flowTypes";
import { OperatorGate } from "./OperatorGate";
import { ReasonFlow } from "./ReasonFlow";

/**
 * "Request changes": asks for a required reason, then POSTs
 * `/requests/{id}/reject` with `{ reason, by }` (sends a spec_review or
 * plan_review request back to its drafting state). Renders nothing when
 * `open` is false or the console cannot write (`useApi().canWrite`): an
 * action the server would 403 is never offered. Prompts for the operator
 * name first when none is stored.
 */
export function RejectDialog({ request, open, onOpenChange, onDone }: FlowProps) {
  const { canWrite } = useApi();
  if (!open || !canWrite) return null;
  return (
    <OperatorGate onOpenChange={onOpenChange}>
      {(by) => (
        <RejectBody requestId={request.id} by={by} onOpenChange={onOpenChange} onDone={onDone} />
      )}
    </OperatorGate>
  );
}

interface BodyProps {
  readonly requestId: string;
  readonly by: string;
  readonly onOpenChange: (open: boolean) => void;
  readonly onDone: FlowProps["onDone"];
}

function RejectBody({ requestId, by, onOpenChange, onDone }: BodyProps) {
  const reject = useRejectRequest(requestId);
  return (
    <ReasonFlow
      title="Request changes"
      confirmLabel="Request changes"
      pending={reject.isPending}
      error={reject.error}
      onOpenChange={onOpenChange}
      write={(reason) => reject.mutateAsync({ reason, by })}
      onDone={onDone}
    />
  );
}
