import { Plus, X } from "lucide-react";
import { useState } from "react";

import type { RejectionAnchor } from "@/domain/request";
import { type AnchorTarget, anchorPlace } from "@/domain/reviewAnchors";
import { Button } from "@/ui/Button";
import { Field, Input, Select } from "@/ui/Input";

export interface AnchoredNotesProps {
  /** The places a note can point at; the component renders nothing when there are none. */
  readonly targets: readonly AnchorTarget[];
  readonly notes: readonly RejectionAnchor[];
  readonly onChange: (notes: readonly RejectionAnchor[]) => void;
  readonly disabled?: boolean;
}

/**
 * Notes on specific places of the document under review: pick a section or
 * a criterion, write what is wrong with it, add. Each is sent with the
 * rejection as a structured anchor, so the redraft is told the exact item
 * and not a sentence it has to locate.
 */
export function AnchoredNotes({ targets, notes, onChange, disabled = false }: AnchoredNotesProps) {
  const [targetId, setTargetId] = useState(targets[0]?.id ?? "");
  const [text, setText] = useState("");
  if (targets.length === 0) return null;
  const target = targets.find((t) => t.id === targetId) ?? targets[0];
  function add() {
    const note = text.trim();
    if (target === undefined || note === "") return;
    onChange([...notes, { path: target.path, section: target.section, item: target.item, note }]);
    setText("");
  }
  return (
    <fieldset className="flex flex-col gap-2" disabled={disabled}>
      <legend className="text-fg mb-1.5 text-xs font-medium">Notes on specific places</legend>
      {notes.length === 0 ? null : (
        <ul aria-label="Anchored notes" className="flex flex-col gap-1">
          {notes.map((note, i) => (
            <li key={i} className="flex items-start gap-2 text-sm">
              <span className="min-w-0 flex-1 break-words">
                <span className="text-fg-muted font-mono text-xs">{anchorPlace(note)}</span>
                {`: ${note.note}`}
              </span>
              <Button
                variant="ghost"
                size="icon"
                aria-label={`Remove note ${i + 1}`}
                onClick={() => {
                  onChange(notes.filter((_, j) => j !== i));
                }}
              >
                <X aria-hidden="true" />
              </Button>
            </li>
          ))}
        </ul>
      )}
      <Field label="Place">
        <Select
          value={target?.id ?? ""}
          onChange={(event) => {
            setTargetId(event.target.value);
          }}
        >
          {targets.map((t) => (
            <option key={t.id} value={t.id}>
              {t.label}
            </option>
          ))}
        </Select>
      </Field>
      <div className="flex items-end gap-2">
        <Field label="Note on this place" className="min-w-0 flex-1">
          <Input
            value={text}
            onChange={(event) => {
              setText(event.target.value);
            }}
            onKeyDown={(event) => {
              if (event.key !== "Enter") return;
              event.preventDefault();
              add();
            }}
          />
        </Field>
        <Button disabled={text.trim() === ""} onClick={add}>
          <Plus aria-hidden="true" />
          Add note
        </Button>
      </div>
    </fieldset>
  );
}
