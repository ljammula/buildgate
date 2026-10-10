import { type ReactNode, useState } from "react";

import { useApi } from "@/api/ApiProvider";
import { useRejectRequest } from "@/api/requestQueries";
import type { RequestSummary } from "@/domain/request";

import type { FlowProps } from "./flowTypes";
import { OperatorGate } from "./OperatorGate";
import { ReasonFlow } from "./ReasonFlow";

/**
 * "Send back": the recovery route to internal/request.SendBack. Required
 * reason plus a target, then POST `/requests/{id}/reject` with
 * `{ reason, by, to }`, `to` being `plan` or `spec`. The "To planning"
 * target is disabled when `request.canSendBackToPlan` is false (the dialog
 * then starts on spec, the only target the server accepts); it also starts
 * on spec for a `spec_conformity` quarantine, where the criterion itself
 * usually needs rewording. Renders nothing when closed or when the console
 * cannot write.
 */
export interface SendBackDialogProps extends FlowProps {
  /**
   * Set when the request is no longer in the stage this dialog was opened on:
   * the send-back cannot be sent, and what was typed stays visible.
   */
  readonly blocked?: ReactNode;
}

export function SendBackDialog({
  request,
  open,
  onOpenChange,
  onDone,
  blocked,
}: SendBackDialogProps) {
  const { canWrite } = useApi();
  if (!open || !canWrite) return null;
  return (
    <OperatorGate onOpenChange={onOpenChange}>
      {(by) => (
        <SendBackBody
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

function SendBackBody({ request, by, onOpenChange, onDone, blocked }: BodyProps) {
  const send = useRejectRequest(request.id);
  // The stage on screen when the dialog opened, fixed: the server refuses the
  // send-back (409) if the request has left it or entered it again since.
  const [seen] = useState({ state: request.state, enteredAt: request.enteredAt });
  const planAllowed = request.canSendBackToPlan;
  const [target, setTarget] = useState<"plan" | "spec">(
    planAllowed && request.quarantineCheck !== "spec_conformity" ? "plan" : "spec",
  );
  return (
    <ReasonFlow
      title="Send back"
      confirmLabel="Send back"
      blocked={blocked}
      pending={send.isPending}
      error={send.error}
      onOpenChange={onOpenChange}
      write={(reason) => send.mutateAsync({ reason, by, to: target, seen })}
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
          To planning
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
          To spec
        </label>
      </fieldset>
    </ReasonFlow>
  );
}
