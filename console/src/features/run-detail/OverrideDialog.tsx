import { useState } from "react";

import type { OverrideRunInput } from "@/api/runs";
import { OperatorGate } from "@/shared/approval/OperatorGate";
import { Button } from "@/ui/Button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/ui/Dialog";
import { Field, Select, Textarea } from "@/ui/Input";

export interface OverrideDialogProps {
  readonly open: boolean;
  readonly onOpenChange: (open: boolean) => void;
  readonly onApply: (values: OverrideRunInput) => void;
  /** The reason the dialog opens with (the request page's quarantine callout knows it); empty when none. */
  readonly initialReason?: string;
}

/**
 * Collects why, and the state to move the quarantined run to. Who is the
 * stored operator name, asked for once by the same prompt every other write
 * uses.
 */
export function OverrideDialog({ onOpenChange, ...rest }: OverrideDialogProps) {
  return (
    <OperatorGate onOpenChange={onOpenChange}>
      {(by) => <OverrideForm by={by} onOpenChange={onOpenChange} {...rest} />}
    </OperatorGate>
  );
}

interface OverrideFormProps extends OverrideDialogProps {
  /** The operator's name, already known. */
  readonly by: string;
}

function OverrideForm({ by, open, onOpenChange, onApply, initialReason = "" }: OverrideFormProps) {
  const [reason, setReason] = useState(initialReason);
  const [state, setState] = useState("accepted");

  function apply() {
    if (reason.trim() === "") return;
    onApply({ by, reason: reason.trim(), state });
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent showClose={false}>
        <DialogHeader>
          <DialogTitle>Override quarantined run</DialogTitle>
          <DialogDescription>
            Records who moved this run out of quarantine, and why.
          </DialogDescription>
        </DialogHeader>
        <div className="flex flex-col gap-3">
          <Field label="Reason">
            <Textarea
              rows={3}
              value={reason}
              onChange={(e) => {
                setReason(e.target.value);
              }}
            />
          </Field>
          <Field label="New state">
            <Select
              value={state}
              onChange={(e) => {
                setState(e.target.value);
              }}
            >
              <option value="accepted">Accepted</option>
              <option value="halted">Halted</option>
            </Select>
          </Field>
        </div>
        <DialogFooter>
          <Button
            variant="ghost"
            onClick={() => {
              onOpenChange(false);
            }}
          >
            Cancel
          </Button>
          <Button variant="primary" onClick={apply}>
            Apply
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
