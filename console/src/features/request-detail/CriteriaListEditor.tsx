import { ArrowDown, ArrowUp, Plus, Trash2 } from "lucide-react";
import { type KeyboardEvent, useRef, useState } from "react";

import {
  addCriterion,
  criteriaItems,
  moveCriterion,
  removeCriterion,
  setCriterionBody,
} from "@/domain/structuredEdit";
import { Button } from "@/ui/Button";
import { Input, Textarea } from "@/ui/Input";

import { useFieldDraft } from "./useFieldDraft";

export interface CriteriaListEditorProps {
  /** The editor's current text: the list is read from it and every action rewrites it. */
  readonly text: string;
  readonly onChange: (text: string) => void;
  readonly disabled?: boolean;
}

interface RowProps {
  readonly index: number;
  readonly count: number;
  readonly body: string;
  readonly disabled: boolean;
  readonly onBody: (body: string) => void;
  readonly onMove: (offset: -1 | 1) => void;
  readonly onRemove: () => void;
  readonly fieldRef: (el: HTMLTextAreaElement | null) => void;
}

function CriterionRow({
  index,
  count,
  body,
  disabled,
  onBody,
  onMove,
  onRemove,
  fieldRef,
}: RowProps) {
  const n = index + 1;
  // An emptied first line is no longer a criterion to the server, so it is
  // held in the field and not written until there is text again.
  const draft = useFieldDraft(body, (next) => {
    if ((next.split("\n")[0] ?? "").trim() !== "") onBody(next);
  });
  function onKeyDown(event: KeyboardEvent<HTMLTextAreaElement>) {
    if (!event.altKey || (event.key !== "ArrowUp" && event.key !== "ArrowDown")) return;
    event.preventDefault();
    onMove(event.key === "ArrowUp" ? -1 : 1);
  }
  return (
    <li className="flex items-start gap-2">
      <span aria-hidden="true" className="text-fg-muted w-6 pt-1.5 text-right font-mono text-xs">
        {`${n}.`}
      </span>
      <Textarea
        ref={fieldRef}
        aria-label={`Criterion ${n}`}
        className="min-h-8 min-w-0 flex-1"
        rows={Math.max(1, draft.value.split("\n").length)}
        readOnly={disabled}
        value={draft.value}
        onChange={(event) => {
          draft.onChange(event.target.value);
        }}
        onBlur={draft.onBlur}
        onKeyDown={onKeyDown}
      />
      <div className="flex shrink-0">
        <Button
          variant="ghost"
          size="icon"
          aria-label={`Move criterion ${n} up`}
          disabled={disabled || index === 0}
          onClick={() => {
            onMove(-1);
          }}
        >
          <ArrowUp aria-hidden="true" />
        </Button>
        <Button
          variant="ghost"
          size="icon"
          aria-label={`Move criterion ${n} down`}
          disabled={disabled || index === count - 1}
          onClick={() => {
            onMove(1);
          }}
        >
          <ArrowDown aria-hidden="true" />
        </Button>
        <Button
          variant="ghost"
          size="icon"
          aria-label={`Remove criterion ${n}`}
          disabled={disabled}
          onClick={onRemove}
        >
          <Trash2 aria-hidden="true" />
        </Button>
      </div>
    </li>
  );
}

/**
 * The spec's acceptance criteria as a list: edit one, add, remove, reorder
 * (Alt+Up/Down in a field), with the numbers kept in step. Each action
 * rewrites the editor's text and only the criteria's own lines in it, so the
 * text box below always holds exactly what Save will send.
 */
export function CriteriaListEditor({ text, onChange, disabled = false }: CriteriaListEditorProps) {
  const items = criteriaItems(text);
  const [adding, setAdding] = useState("");
  const fields = useRef(new Map<number, HTMLTextAreaElement>());
  if (items === null) {
    return (
      <p data-testid="criteria-list-editor" className="text-fg-muted text-xs">
        {'Add a "## Acceptance criteria" heading to edit the criteria as a list.'}
      </p>
    );
  }
  function add() {
    const body = adding.trim();
    if (body === "") return;
    onChange(addCriterion(text, body));
    setAdding("");
  }
  function move(index: number, offset: -1 | 1) {
    const target = index + offset;
    if (items === null || target < 0 || target >= items.length) return;
    onChange(moveCriterion(text, index, offset));
    fields.current.get(target)?.focus();
  }
  return (
    <section
      aria-label="Acceptance criteria"
      data-testid="criteria-list-editor"
      className="flex flex-col gap-2"
    >
      <h4 className="text-fg-muted text-xs font-semibold">{`Acceptance criteria (${items.length})`}</h4>
      <ol className="flex flex-col gap-1.5">
        {items.map((item, index) => (
          <CriterionRow
            // By position: after a move the field at this position shows its new criterion.
            key={index}
            index={index}
            count={items.length}
            body={item.body}
            disabled={disabled}
            onBody={(body) => {
              onChange(setCriterionBody(text, index, body));
            }}
            onMove={(offset) => {
              move(index, offset);
            }}
            onRemove={() => {
              onChange(removeCriterion(text, index));
            }}
            fieldRef={(el) => {
              if (el === null) fields.current.delete(index);
              else fields.current.set(index, el);
            }}
          />
        ))}
      </ol>
      <div className="flex items-center gap-2 pl-8">
        <Input
          aria-label="New criterion"
          placeholder="Add a criterion, then Enter"
          className="min-w-0 flex-1"
          readOnly={disabled}
          value={adding}
          onChange={(event) => {
            setAdding(event.target.value);
          }}
          onKeyDown={(event) => {
            if (event.key !== "Enter") return;
            event.preventDefault();
            add();
          }}
        />
        <Button size="sm" disabled={disabled || adding.trim() === ""} onClick={add}>
          <Plus aria-hidden="true" />
          Add
        </Button>
      </div>
    </section>
  );
}
