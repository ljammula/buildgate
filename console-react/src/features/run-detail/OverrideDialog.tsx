import { useState } from "react";

import type { OverrideRunInput } from "@/api/runs";
import { Button } from "@/ui/Button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/ui/Dialog";
import { Field, Input, Select } from "@/ui/Input";

export interface OverrideDialogProps {
  readonly open: boolean;
  readonly onOpenChange: (open: boolean) => void;
  readonly onApply: (values: OverrideRunInput) => void;
}

/** Collects who is overriding, why, and the state to move the quarantined run to. */
export function OverrideDialog({ open, onOpenChange, onApply }: OverrideDialogProps) {
  const [by, setBy] = useState("");
  const [reason, setReason] = useState("");
  const [state, setState] = useState("accepted");

  function apply() {
    if (by.trim() === "" || reason.trim() === "") return;
    onApply({ by: by.trim(), reason: reason.trim(), state });
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
          <Field label="Operator">
            <Input
              value={by}
              onChange={(e) => {
                setBy(e.target.value);
              }}
            />
          </Field>
          <Field label="Reason">
            <Input
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
