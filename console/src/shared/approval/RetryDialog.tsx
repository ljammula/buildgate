import { useState } from "react";

import { useApi } from "@/api/ApiProvider";
import { useRetryRequest } from "@/api/requestQueries";
import { Checkbox } from "@/ui/Input";

import type { FlowProps } from "./flowTypes";
import { OperatorGate } from "./OperatorGate";
import { ReasonFlow } from "./ReasonFlow";

/**
 * "Retry this request": recovery for a halted or quarantined request.
 * Required reason, then POST `/requests/{id}/retry` with `{ reason, by }`,
 * plus `from: "scratch"` when the operator ticks "Retry from scratch"
 * (`factoryd retry -from scratch`). Renders nothing when closed or when the
 * console cannot write.
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
  const [fromScratch, setFromScratch] = useState(false);
  return (
    <ReasonFlow
      title="Retry this request"
      confirmLabel="Retry"
      pending={retry.isPending}
      error={retry.error}
      onOpenChange={onOpenChange}
      write={(reason) => retry.mutateAsync({ reason, by, fromScratch })}
      onDone={onDone}
    >
      <div className="flex flex-col gap-1 text-sm text-fg">
        <label className="flex items-center gap-2">
          <Checkbox
            checked={fromScratch}
            aria-describedby="retry-from-scratch-hint"
            onChange={(event) => {
              setFromScratch(event.target.checked);
            }}
          />
          Retry from scratch
        </label>
        <p id="retry-from-scratch-hint" className="text-xs text-fg-muted">
          Rebuild the ticket from the base commit instead of continuing from the failed
          attempt&apos;s commit.
        </p>
      </div>
    </ReasonFlow>
  );
}
