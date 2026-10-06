import { useState } from "react";

import { useApi } from "@/api/ApiProvider";
import { useRejectRequest } from "@/api/requestQueries";
import type { RequestSummary } from "@/domain/request";

import type { FlowProps } from "./flowTypes";
import { OperatorGate } from "./OperatorGate";
import { ReasonFlow } from "./ReasonFlow";

/**
 * "Send back": the recovery route to internal/request.SendBack. Required
 * reason plus a target, then POST `/requests/{id}/reject` with
 * `{ reason, by, to }`, `to` being `plan` or `spec`. The "Back to planning"
 * target is disabled when `request.canSendBackToPlan` is false (the dialog
 * then starts on spec, the only target the server accepts); it also starts
 * on spec for a `spec_conformity` quarantine, where the criterion itself
 * usually needs rewording. Renders nothing when closed or when the console
 * cannot write.
 */
export function SendBackDialog({ request, open, onOpenChange, onDone }: FlowProps) {
  const { canWrite } = useApi();
  if (!open || !canWrite) return null;
  return (
    <OperatorGate onOpenChange={onOpenChange}>
      {(by) => (
        <SendBackBody request={request} by={by} onOpenChange={onOpenChange} onDone={onDone} />
      )}
    </OperatorGate>
  );
}

interface BodyProps {
  readonly request: RequestSummary;
  readonly by: string;
  readonly onOpenChange: (open: boolean) => void;
  readonly onDone: FlowProps["onDone"];
}

function SendBackBody({ request, by, onOpenChange, onDone }: BodyProps) {
  const send = useRejectRequest(request.id);
  const planAllowed = request.canSendBackToPlan;
  const [target, setTarget] = useState<"plan" | "spec">(
    planAllowed && request.quarantineCheck !== "spec_conformity" ? "plan" : "spec",
  );
  return (
    <ReasonFlow
      title="Send back"
      confirmLabel="Send back"
      pending={send.isPending}
      error={send.error}
      onOpenChange={onOpenChange}
      write={(reason) => send.mutateAsync({ reason, by, to: target })}
      onDone={onDone}
    >
      <fieldset className="flex flex-col gap-1.5 text-sm text-fg">
        <legend className="sr-only">Send back target</legend>
        <label className="flex items-center gap-2">
          <input
            type="radio"
            name="send-back-target"
            value="plan"
            checked={target === "plan"}
            disabled={!planAllowed}
            onChange={() => {
              setTarget("plan");
            }}
          />
          Back to planning
        </label>
        <label className="flex items-center gap-2">
          <input
            type="radio"
            name="send-back-target"
            value="spec"
            checked={target === "spec"}
            onChange={() => {
              setTarget("spec");
            }}
          />
          Back to spec drafting
        </label>
      </fieldset>
    </ReasonFlow>
  );
}
