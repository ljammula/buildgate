import { Loader2 } from "lucide-react";
import type { ReactNode } from "react";

import { ApiError } from "@/domain/apiError";
import { Button } from "@/ui/Button";
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/ui/Dialog";
import { ErrorCallout } from "@/ui/ErrorDisplay";

export interface FlowDialogProps {
  /** Whether the dialog is shown. The flow components below pass `true` and are mounted only while open. */
  readonly open: boolean;
  /** Called with `false` for Cancel/Escape/outside-click. Ignored while `pending`. */
  readonly onOpenChange: (open: boolean) => void;
  readonly title: string;
  /** Body copy under the title. Omit for a form-only dialog. */
  readonly description?: ReactNode;
  /** The form controls or extra lines between the description and the footer. */
  readonly children?: ReactNode;
  readonly confirmLabel: string;
  /** Label of the dismiss button; default "Cancel". */
  readonly cancelLabel?: string;
  readonly tone?: "primary" | "danger";
  /** The confirm button is disabled (a required field is empty). */
  readonly confirmDisabled?: boolean;
  /** A write is in flight: confirm shows a spinner and everything is disabled. */
  readonly pending: boolean;
  /** The last write's failure; shown with the server's own message while the dialog stays open. */
  readonly error?: unknown;
  /** Clicking outside closes the dialog (default false: typed text is never silently discarded). */
  readonly dismissOnOutside?: boolean;
  readonly onConfirm: () => void;
}

/**
 * The shell every approval flow uses: title, optional description, body,
 * the error callout, and Cancel / confirm buttons. The confirm button is
 * disabled while a write is in flight, and the dialog cannot be dismissed
 * then either.
 */
export function FlowDialog({
  open,
  onOpenChange,
  title,
  description,
  children,
  confirmLabel,
  cancelLabel = "Cancel",
  tone = "primary",
  confirmDisabled = false,
  pending,
  error,
  dismissOnOutside = false,
  onConfirm,
}: FlowDialogProps) {
  const hasError = error !== undefined && error !== null;
  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (!pending) onOpenChange(next);
      }}
    >
      <DialogContent
        showClose={false}
        {...(description === undefined ? { "aria-describedby": undefined } : {})}
        onInteractOutside={(event) => {
          if (!dismissOnOutside) event.preventDefault();
        }}
      >
        <DialogHeader>
          <DialogTitle>{title}</DialogTitle>
          {description === undefined ? null : (
            <DialogDescription asChild>
              <div>{description}</div>
            </DialogDescription>
          )}
        </DialogHeader>
        {children}
        {hasError ? <ErrorCallout error={error} /> : null}
        {/* A 503 is the server failing to check, not refusing: say that trying again may work. */}
        {error instanceof ApiError && error.isRetryable ? (
          <p className="text-fg-muted text-sm">(temporary: try again)</p>
        ) : null}
        <DialogFooter>
          <DialogClose asChild>
            <Button variant="ghost" disabled={pending}>
              {cancelLabel}
            </Button>
          </DialogClose>
          <Button
            variant={tone === "danger" ? "danger" : "primary"}
            disabled={confirmDisabled || pending}
            onClick={onConfirm}
          >
            {pending ? (
              <Loader2 className="animate-spin motion-reduce:animate-none" aria-hidden="true" />
            ) : null}
            {confirmLabel}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
