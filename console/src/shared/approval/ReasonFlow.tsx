import { type ReactNode, useState } from "react";

import type { RequestSummary } from "@/domain/request";
import { Field, Textarea } from "@/ui/Input";

import { FlowDialog } from "./FlowDialog";

export interface ReasonFlowProps {
  readonly title: string;
  readonly confirmLabel: string;
  readonly tone?: "primary" | "danger";
  /** One line under the title: what confirming does. */
  readonly description?: ReactNode;
  /** Extra controls under the reason field (the send-back target). */
  readonly children?: ReactNode;
  readonly pending: boolean;
  readonly error: unknown;
  readonly onOpenChange: (open: boolean) => void;
  /** Sends the trimmed, non-empty reason; the returned promise settles with the new record. */
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
  pending,
  error,
  onOpenChange,
  write,
  onDone,
}: ReasonFlowProps) {
  const [reason, setReason] = useState("");
  const trimmed = reason.trim();
  async function confirm() {
    if (trimmed === "") return;
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
      confirmDisabled={trimmed === ""}
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
    </FlowDialog>
  );
}
