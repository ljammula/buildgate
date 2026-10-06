import { type ReactNode, useState } from "react";

import { getOperatorName, setOperatorName } from "@/platform/operatorIdentity";
import { Field, Input } from "@/ui/Input";

import { FlowDialog } from "./FlowDialog";

export interface OperatorGateProps {
  /** Called with `false` when the operator cancels the name prompt. */
  readonly onOpenChange: (open: boolean) => void;
  /**
   * Called with the trimmed name when the operator types one into the prompt
   * (not when a stored name is reused), after it is stored. For a flow that
   * sends its write itself, rather than rendering a body once the name is known.
   */
  readonly onNamed?: (name: string) => void;
  /** Rendered once an operator name is known; receives it (the `by` of the write). */
  readonly children?: (by: string) => ReactNode;
}

/**
 * Resolves the operator's stored display name, prompting once with the
 * "Your name" dialog when none is stored. Cancelling the prompt closes the
 * whole flow: an empty identity is never a valid `by`. Mount it only while
 * the flow is open, so the stored name is re-read on every open.
 */
export function OperatorGate({ onOpenChange, onNamed, children }: OperatorGateProps) {
  const [by, setBy] = useState(() => {
    const stored = getOperatorName();
    return stored !== null && stored !== "" ? stored : "";
  });
  const [entered, setEntered] = useState("");
  if (by !== "") return children === undefined ? null : <>{children(by)}</>;
  const value = entered.trim();
  const confirm = () => {
    if (value === "") return;
    setOperatorName(value);
    setBy(value);
    onNamed?.(value);
  };
  return (
    <FlowDialog
      open
      onOpenChange={onOpenChange}
      title="Your name"
      confirmLabel="Continue"
      confirmDisabled={value === ""}
      pending={false}
      onConfirm={confirm}
    >
      <Field label="Operator name" hint="Recorded on every approve/reject you make from here.">
        <Input
          autoFocus
          value={entered}
          onChange={(event) => {
            setEntered(event.target.value);
          }}
          onKeyDown={(event) => {
            // Enter continues, as in any one-field prompt.
            if (event.key === "Enter" && !event.nativeEvent.isComposing) confirm();
          }}
        />
      </Field>
    </FlowDialog>
  );
}
