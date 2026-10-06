import { useEffect, useState } from "react";

import { sectionBodyProblem, sections, setSectionBody } from "@/domain/structuredEdit";
import { requiredSpecHeadings, requiredTicketHeadings } from "@/domain/specSkeleton";
import { Button } from "@/ui/Button";
import { Textarea } from "@/ui/Input";

import type { StructureKind } from "./StructureChecklist";

export interface SectionEditorProps {
  readonly kind: StructureKind;
  /** The editor's current text: bodies are read from it and Apply rewrites it. */
  readonly text: string;
  readonly onChange: (text: string) => void;
  /**
   * Told whether a section's field is open with text not yet applied: Save
   * sends the editor's text, so an open field's words would be left behind.
   */
  readonly onPendingChange?: (pending: boolean) => void;
  readonly disabled?: boolean;
}

const name = (heading: string) => heading.replace(/^#+\s*/, "");

interface RowProps {
  readonly heading: string;
  readonly headings: readonly string[];
  readonly body: string;
  readonly present: boolean;
  readonly text: string;
  readonly onChange: (text: string) => void;
  readonly onOpenChange: (heading: string, open: boolean) => void;
  readonly disabled: boolean;
}

function SectionRow({
  heading,
  headings,
  body,
  present,
  text,
  onChange,
  onOpenChange,
  disabled,
}: RowProps) {
  const label = name(heading);
  const [draft, setDraftState] = useState<string | null>(null);
  // The section's text when the field opened: Apply replaces the section as
  // it is now, so text that changed underneath would be lost unseen.
  const [opened, setOpened] = useState("");
  const setDraft = (next: string | null) => {
    setDraftState(next);
    onOpenChange(heading, next !== null);
  };
  const shown = body.replace(/\r(?=\n|$)/g, "");
  const moved = !present
    ? "This section's heading is no longer in the text below, so there is nowhere to apply this. Copy what you need, then Cancel."
    : shown !== opened
      ? "This section changed in the text below since this field opened; applying would overwrite that. Copy what you need, Cancel, and open it again."
      : null;
  const problem = draft === null ? null : (moved ?? sectionBodyProblem(headings, draft, heading));
  return (
    <li className="flex flex-col gap-1">
      <div className="flex items-center gap-2">
        <span className="font-mono text-xs">{label}</span>
        {!present && draft === null ? (
          <span className="text-fg-muted text-xs">missing</span>
        ) : draft === null ? (
          <Button
            size="sm"
            variant="ghost"
            disabled={disabled}
            aria-label={`Edit section ${label}`}
            onClick={() => {
              setOpened(shown);
              setDraft(shown);
            }}
          >
            Edit
          </Button>
        ) : null}
      </div>
      {draft === null ? null : (
        <div className="flex flex-col gap-1">
          <Textarea
            mono
            aria-label={`Section ${label}`}
            rows={Math.max(3, draft.split("\n").length)}
            value={draft}
            onChange={(event) => {
              setDraft(event.target.value);
            }}
          />
          {problem === null ? null : <p className="text-tone-danger text-xs">{problem}</p>}
          <div className="flex gap-2">
            <Button
              size="sm"
              variant="primary"
              disabled={disabled || problem !== null}
              aria-label={`Apply section ${label}`}
              onClick={() => {
                onChange(setSectionBody(text, headings, heading, draft));
                setDraft(null);
              }}
            >
              Apply
            </Button>
            <Button
              size="sm"
              variant="ghost"
              aria-label={`Cancel section ${label}`}
              onClick={() => {
                setDraft(null);
              }}
            >
              Cancel
            </Button>
          </div>
        </div>
      )}
    </li>
  );
}

/**
 * One Edit per required section of the file: a section's body is edited in
 * its own field and Apply writes it into the editor's text (never to the
 * server); the rest of the text comes back byte for byte. Bodies are shown
 * only as a field's value.
 */
export function SectionEditor({
  kind,
  text,
  onChange,
  onPendingChange,
  disabled = false,
}: SectionEditorProps) {
  const headings = kind === "spec" ? requiredSpecHeadings : requiredTicketHeadings;
  const [open, setOpen] = useState<readonly string[]>([]);
  const pending = open.length > 0;
  useEffect(() => {
    onPendingChange?.(pending);
  }, [pending, onPendingChange]);
  return (
    <section aria-label="Sections" className="flex flex-col gap-1">
      <h4 className="text-fg-muted text-xs font-semibold">Sections</h4>
      <ul className="flex flex-col gap-1">
        {sections(text, headings).map((slice) => (
          <SectionRow
            key={slice.heading}
            heading={slice.heading}
            headings={headings}
            body={slice.body}
            present={slice.present}
            text={text}
            onChange={onChange}
            onOpenChange={(heading, isOpen) => {
              setOpen((was) => {
                const rest = was.filter((h) => h !== heading);
                return isOpen ? [...rest, heading] : rest;
              });
            }}
            disabled={disabled}
          />
        ))}
      </ul>
    </section>
  );
}
