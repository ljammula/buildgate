import { useApi } from "@/api/ApiProvider";
import { useCancelRequest } from "@/api/requestQueries";

import type { FlowProps } from "./flowTypes";
import { OperatorGate } from "./OperatorGate";
import { ReasonFlow } from "./ReasonFlow";

/**
 * "Cancel this request": recovery for a halted or quarantined request.
 * Required reason, then POST `/requests/{id}/cancel` with `{ reason, by }`.
 * Renders nothing when closed or when the console cannot write.
 */
export function CancelDialog({ request, open, onOpenChange, onDone }: FlowProps) {
  const { canWrite } = useApi();
  if (!open || !canWrite) return null;
  return (
    <OperatorGate onOpenChange={onOpenChange}>
      {(by) => (
        <CancelBody requestId={request.id} by={by} onOpenChange={onOpenChange} onDone={onDone} />
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

function CancelBody({ requestId, by, onOpenChange, onDone }: BodyProps) {
  const cancel = useCancelRequest(requestId);
  return (
    <ReasonFlow
      title="Cancel this request"
      confirmLabel="Cancel request"
      tone="danger"
      pending={cancel.isPending}
      error={cancel.error}
      onOpenChange={onOpenChange}
      write={(reason) => cancel.mutateAsync({ reason, by })}
      onDone={onDone}
    />
  );
}
