import { useApi } from "@/api/ApiProvider";
import { useRetryRequest } from "@/api/requestQueries";

import type { FlowProps } from "./flowTypes";
import { OperatorGate } from "./OperatorGate";
import { ReasonFlow } from "./ReasonFlow";

/**
 * "Retry this request": recovery for a halted or quarantined request.
 * Required reason, then POST `/requests/{id}/retry` with `{ reason, by }`.
 * Renders nothing when closed or when the console cannot write.
 */
export function RetryDialog({ request, open, onOpenChange, onDone }: FlowProps) {
  const { canWrite } = useApi();
  if (!open || !canWrite) return null;
  return (
    <OperatorGate onOpenChange={onOpenChange}>
      {(by) => (
        <RetryBody requestId={request.id} by={by} onOpenChange={onOpenChange} onDone={onDone} />
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

function RetryBody({ requestId, by, onOpenChange, onDone }: BodyProps) {
  const retry = useRetryRequest(requestId);
  return (
    <ReasonFlow
      title="Retry this request"
      confirmLabel="Retry"
      pending={retry.isPending}
      error={retry.error}
      onOpenChange={onOpenChange}
      write={(reason) => retry.mutateAsync({ reason, by })}
      onDone={onDone}
    />
  );
}
