import { requiredTicketHeaderKeys, validateTicketPlan } from "@/domain/specSkeleton";
import {
  headerList,
  setCoveredCriteria,
  setTicketHeaderValue,
  ticketHeaderValue,
} from "@/domain/structuredEdit";
import { Checkbox, Field, Input } from "@/ui/Input";

import { useFieldDraft } from "./useFieldDraft";

export interface TicketFieldsEditorProps {
  /** The editor's current text: the fields are read from it and every change rewrites it. */
  readonly text: string;
  readonly onChange: (text: string) => void;
  /** The spec's numbered criteria as the server reads them ("1. It works."), in order. */
  readonly specCriteria: readonly string[];
  readonly disabled?: boolean;
}

interface HeaderFieldProps {
  readonly headerKey: string;
  readonly label: string;
  /** A comma-separated list of paths: its entries are counted under the field. */
  readonly list?: boolean;
  readonly text: string;
  readonly onChange: (text: string) => void;
  readonly disabled: boolean;
}

function HeaderField({
  headerKey,
  label,
  list = false,
  text,
  onChange,
  disabled,
}: HeaderFieldProps) {
  const current = ticketHeaderValue(text, headerKey);
  const draft = useFieldDraft(current ?? "", (next) => {
    onChange(setTicketHeaderValue(text, headerKey, next.trim(), requiredTicketHeaderKeys));
  });
  const entries = headerList(draft.value).length;
  const hint =
    current === null
      ? `No ${headerKey} line yet: typing here adds it.`
      : list
        ? `${entries} ${entries === 1 ? "path" : "paths"}, comma-separated`
        : undefined;
  return (
    <Field label={label} hint={hint}>
      <Input
        className="font-mono text-xs"
        readOnly={disabled}
        value={draft.value}
        onChange={(event) => {
          draft.onChange(event.target.value);
        }}
        onBlur={draft.onBlur}
      />
    </Field>
  );
}

/**
 * A ticket's three header lines as inputs and its covered criteria as
 * checkboxes against the spec's own list. Each change rewrites the editor's
 * text and only the line or list it names; what the server makes of the
 * values (path rules, scope) is its 422 on Save, shown as ever.
 */
export function TicketFieldsEditor({
  text,
  onChange,
  specCriteria,
  disabled = false,
}: TicketFieldsEditorProps) {
  const plan = validateTicketPlan(text);
  const hasSection = plan.missingHeading === null;
  const covered = new Set(plan.covered);
  const stray = plan.covered.filter((n) => n < 1 || n > specCriteria.length);
  const options = [
    ...specCriteria.map((label, i) => ({ number: i + 1, label })),
    ...[...new Set(stray)].map((number) => ({ number, label: `${number} (not in the spec)` })),
  ];
  function toggle(number: number, on: boolean) {
    const next = new Set(covered);
    if (on) next.add(number);
    else next.delete(number);
    onChange(
      setCoveredCriteria(
        text,
        [...next].sort((a, b) => a - b),
      ),
    );
  }
  const shared = { text, onChange, disabled };
  return (
    <section
      aria-label="Ticket fields"
      data-testid="ticket-fields-editor"
      className="flex flex-col gap-3"
    >
      <HeaderField headerKey="Verify-Command:" label="Verify command" {...shared} />
      <HeaderField headerKey="Allowed-Files:" label="Allowed files" list {...shared} />
      <HeaderField
        headerKey="Required-Changed-Files:"
        label="Required changed files"
        list
        {...shared}
      />
      {options.length === 0 ? null : (
        <fieldset className="flex flex-col gap-1" disabled={disabled || !hasSection}>
          <legend className="text-fg mb-1 text-xs font-medium">Acceptance criteria covered</legend>
          {hasSection ? null : (
            <p className="text-fg-muted text-xs">
              The ticket needs its plan headings before criteria can be ticked here.
            </p>
          )}
          {options.map((option) => (
            <label key={option.number} className="flex items-start gap-2 text-sm">
              <Checkbox
                className="mt-0.5"
                checked={covered.has(option.number)}
                onChange={(event) => {
                  toggle(option.number, event.target.checked);
                }}
              />
              <span className="min-w-0 break-words">{option.label}</span>
            </label>
          ))}
        </fieldset>
      )}
    </section>
  );
}
