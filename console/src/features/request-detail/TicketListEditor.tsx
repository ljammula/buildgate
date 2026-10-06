import { ArrowDown, ArrowUp, Plus, Trash2 } from "lucide-react";
import { type KeyboardEvent, useRef, useState } from "react";

import {
  addSectionListItem,
  moveSectionListItem,
  removeSectionListItem,
  sectionBodyProblem,
  sectionListItems,
  setSectionListItem,
} from "@/domain/structuredEdit";
import { requiredTicketHeadings, trimSpace } from "@/domain/specSkeleton";
import { Button } from "@/ui/Button";
import { Input, Textarea } from "@/ui/Input";

import { useFieldDraft } from "./useFieldDraft";

export type TicketListHeading = "### Steps" | "### Files to touch";

export interface TicketListEditorProps {
  readonly text: string;
  readonly heading: TicketListHeading;
  readonly onChange: (text: string) => void;
  readonly disabled?: boolean;
}

interface RowProps {
  readonly heading: TicketListHeading;
  readonly index: number;
  readonly count: number;
  readonly body: string;
  readonly disabled: boolean;
  readonly onBody: (body: string) => void;
  readonly onMove: (offset: -1 | 1) => void;
  readonly onRemove: () => void;
  readonly fieldRef: (el: HTMLTextAreaElement | null) => void;
}

function TicketListRow({
  heading,
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
  const singular = heading === "### Steps" ? "step" : "file";
  // A line reading as a required heading would move the file's sections, so
  // it is never written through: the field keeps it and says why.
  const [problem, setProblem] = useState<string | null>(null);
  const draft = useFieldDraft(body, (next) => {
    const refused = sectionBodyProblem(requiredTicketHeadings, next);
    setProblem(refused);
    if (refused === null && trimSpace(next.split("\n")[0] ?? "") !== "") onBody(next);
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
      <div className="flex min-w-0 flex-1 flex-col gap-1">
        <Textarea
          ref={fieldRef}
          aria-label={`${singular === "step" ? "Step" : "File"} ${n}`}
          className="min-h-8"
          rows={Math.max(1, draft.value.split("\n").length)}
          readOnly={disabled}
          value={draft.value}
          onChange={(event) => {
            draft.onChange(event.target.value);
          }}
          onBlur={() => {
            draft.onBlur();
            setProblem(null);
          }}
          onKeyDown={onKeyDown}
        />
        {problem === null ? null : <p className="text-tone-danger text-xs">{problem}</p>}
      </div>
      <div className="flex shrink-0">
        <Button
          variant="ghost"
          size="icon"
          aria-label={`Move ${singular} ${n} up`}
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
          aria-label={`Move ${singular} ${n} down`}
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
          aria-label={`Remove ${singular} ${n}`}
          disabled={disabled}
          onClick={onRemove}
        >
          <Trash2 aria-hidden="true" />
        </Button>
      </div>
    </li>
  );
}

/** A ticket plan list editor that rewrites the shared draft text, never saving on its own. */
export function TicketListEditor({
  text,
  heading,
  onChange,
  disabled = false,
}: TicketListEditorProps) {
  const items = sectionListItems(text, heading);
  const [adding, setAdding] = useState("");
  const fields = useRef(new Map<number, HTMLTextAreaElement>());
  const noun = heading === "### Steps" ? "step" : "file";
  const title = heading === "### Steps" ? "Steps" : "Files to touch";
  if (items === null) {
    return (
      <p data-testid={`ticket-list-editor-${noun}`} className="text-fg-muted text-xs">
        {`Add a "${heading}" heading to edit ${title.toLowerCase()} as a list.`}
      </p>
    );
  }
  function add() {
    const body = adding.trim();
    if (body === "") return;
    onChange(addSectionListItem(text, heading, body));
    setAdding("");
  }
  function move(index: number, offset: -1 | 1) {
    const target = index + offset;
    if (items === null || target < 0 || target >= items.length) return;
    onChange(moveSectionListItem(text, heading, index, offset));
    fields.current.get(target)?.focus();
  }
  return (
    <section
      aria-label={title}
      data-testid={`ticket-list-editor-${noun}`}
      className="flex flex-col gap-2"
    >
      <h4 className="text-fg-muted text-xs font-semibold">{`${title} (${items.length})`}</h4>
      <ol className="flex flex-col gap-1.5">
        {items.map((item, index) => (
          <TicketListRow
            key={index}
            heading={heading}
            index={index}
            count={items.length}
            body={item.body}
            disabled={disabled}
            onBody={(body) => {
              onChange(setSectionListItem(text, heading, index, body));
            }}
            onMove={(offset) => {
              move(index, offset);
            }}
            onRemove={() => {
              onChange(removeSectionListItem(text, heading, index));
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
          aria-label={`New ${noun}`}
          placeholder={`Add a ${noun}, then Enter`}
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
          {`Add ${noun}`}
        </Button>
      </div>
    </section>
  );
}
