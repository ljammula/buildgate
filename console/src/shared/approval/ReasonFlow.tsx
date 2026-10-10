import { type ReactNode, useState } from "react";

import type { RequestSummary } from "@/domain/request";
import { Callout } from "@/ui/Feedback";
import { Field, Textarea } from "@/ui/Input";

import { FlowDialog } from "./FlowDialog";

export interface ReasonFlowProps {
  readonly title: string;
  readonly confirmLabel: string;
  readonly tone?: "primary" | "danger";
  /** One line under the title: what confirming does. */
  readonly description?: ReactNode;
  /** Extra controls under the reason field (the send-back target, the anchored notes). */
  readonly children?: ReactNode;
  /** The reason may be left empty: something in `children` carries the feedback instead. */
  readonly reasonOptional?: boolean;
  /**
   * Why nothing may be sent any more (the request changed under the dialog).
   * Shown above the buttons, with confirm disabled; the typed reason and
   * notes stay on screen so the operator can copy them.
   */
  readonly blocked?: ReactNode;
  readonly pending: boolean;
  readonly error: unknown;
  readonly onOpenChange: (open: boolean) => void;
  /** Sends the trimmed reason (non-empty unless `reasonOptional`); the returned promise settles with the new record. */
  readonly write: (reason: string) => Promise<RequestSummary>;
  readonly onDone?: ((request: RequestSummary) => void) | undefined;
}

/**
 * The "reason required, multi-line, not dismissed by an outside click"
 * dialog behind Request changes, Retry, Cancel and Send back. Confirm stays
 * disabled until the reason has text; a failed write leaves the dialog open
 * with the typed reason intact.
 */
export function ReasonFlow({
  title,
  confirmLabel,
  tone = "primary",
  description,
  children,
  reasonOptional = false,
  blocked,
  pending,
  error,
  onOpenChange,
  write,
  onDone,
}: ReasonFlowProps) {
  const [reason, setReason] = useState("");
  const trimmed = reason.trim();
  const missing = trimmed === "" && !reasonOptional;
  const isBlocked = blocked !== undefined && blocked !== null;
  async function confirm() {
    if (missing || isBlocked) return;
    try {
      const updated = await write(trimmed);
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
      title={title}
      {...(description === undefined ? {} : { description })}
      confirmLabel={confirmLabel}
      tone={tone}
      confirmDisabled={missing || isBlocked}
      pending={pending}
      error={error}
      onConfirm={() => void confirm()}
    >
      <Field label="Reason">
        <Textarea
          autoFocus
          rows={3}
          value={reason}
          onChange={(event) => {
            setReason(event.target.value);
          }}
        />
      </Field>
      {children}
      {isBlocked ? (
        <Callout tone="warning" data-testid="flow-blocked">
          {blocked}
        </Callout>
      ) : null}
    </FlowDialog>
  );
}
