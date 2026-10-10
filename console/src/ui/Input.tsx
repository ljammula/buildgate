import {
  type InputHTMLAttributes,
  type ReactElement,
  type ReactNode,
  type Ref,
  type SelectHTMLAttributes,
  type TextareaHTMLAttributes,
  cloneElement,
  useId,
} from "react";

import { cn } from "@/ui/cn";

const controlBase =
  "w-full rounded-md border border-border-control bg-field px-2.5 text-sm text-fg placeholder:text-fg-subtle transition-colors hover:border-fg-subtle disabled:cursor-not-allowed disabled:opacity-50 aria-[invalid=true]:border-tone-danger-border";

export interface InputProps extends InputHTMLAttributes<HTMLInputElement> {
  readonly ref?: Ref<HTMLInputElement>;
}

export function Input({ className, ...props }: InputProps) {
  return <input className={cn(controlBase, "h-8", className)} {...props} />;
}

export interface TextareaProps extends TextareaHTMLAttributes<HTMLTextAreaElement> {
  /** Monospace, for editing specs and plans. */
  readonly mono?: boolean;
  readonly ref?: Ref<HTMLTextAreaElement>;
}

export function Textarea({ mono = false, className, ...props }: TextareaProps) {
  return (
    <textarea
      className={cn(controlBase, "min-h-20 py-1.5", mono && "font-mono text-xs", className)}
      {...props}
    />
  );
}

export interface SelectProps extends SelectHTMLAttributes<HTMLSelectElement> {
  readonly ref?: Ref<HTMLSelectElement>;
}

export function Select({ className, ...props }: SelectProps) {
  return <select className={cn(controlBase, "h-8", className)} {...props} />;
}

export interface CheckboxProps extends Omit<InputHTMLAttributes<HTMLInputElement>, "type"> {
  readonly ref?: Ref<HTMLInputElement>;
}

export function Checkbox({ className, ...props }: CheckboxProps) {
  return (
    <input
      type="checkbox"
      className={cn("size-4 shrink-0 rounded-sm border-border-control accent-accent", className)}
      {...props}
    />
  );
}

interface ControlA11yProps {
  id?: string;
  "aria-describedby"?: string | undefined;
  "aria-invalid"?: boolean;
}

export interface FieldProps {
  readonly label: ReactNode;
  readonly hint?: ReactNode;
  readonly error?: ReactNode;
  /** A single control (Input, Textarea, Select, Checkbox); Field wires its id and ARIA. */
  readonly children: ReactElement<ControlA11yProps>;
  readonly className?: string;
}

/** The form wrapper's other name: `Field` also names a label/value row elsewhere, `FormField` does not. */
export { Field as FormField };

export function Field({ label, hint, error, children, className }: FieldProps) {
  const id = useId();
  const controlId = children.props.id ?? id;
  const hintId = `${controlId}-hint`;
  const errorId = `${controlId}-error`;
  const describedBy =
    [hint ? hintId : undefined, error ? errorId : undefined].filter(Boolean).join(" ") || undefined;
  const control = cloneElement(children, {
    id: controlId,
    "aria-describedby": describedBy,
    ...(error ? { "aria-invalid": true } : {}),
  });
  return (
    <div className={cn("flex flex-col gap-1.5", className)}>
      <label htmlFor={controlId} className="text-xs font-medium text-fg">
        {label}
      </label>
      {control}
      {hint ? (
        <p id={hintId} className="text-xs text-fg-muted">
          {hint}
        </p>
      ) : null}
      {error ? (
        <p id={errorId} className="text-xs text-tone-danger">
          {error}
        </p>
      ) : null}
    </div>
  );
}
