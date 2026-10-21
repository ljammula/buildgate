import { MessageSquarePlus } from "lucide-react";
import { useState } from "react";

import type { RejectionAnchor } from "@/domain/request";
import { anchorForSelection, clipQuote } from "@/domain/reviewAnchors";
import { Button } from "@/ui/Button";
import { Field, Input } from "@/ui/Input";

export interface PassageComposerProps {
  /** The request-relative path of the file the selection is in. */
  readonly path: string;
  /** The file's text exactly as shown. */
  readonly content: string;
  /** The text the reader selected in the raw view. */
  readonly selected: string;
  /** Where the selection starts in `content`, when the page could tell; null otherwise. */
  readonly offset: number | null;
  readonly onAdd: (comment: RejectionAnchor) => void;
  /** The comment was added or dropped: forget the selection. */
  readonly onDone: () => void;
}

/**
 * A selected passage of a reviewed file and a note on it. Offers "Comment on
 * selection"; the note is added to the pending batch with the nearest
 * section heading, the criterion number where the passage is in one, and the
 * passage itself, which the server checks is still there when the batch is
 * sent.
 */
export function PassageComposer({
  path,
  content,
  selected,
  offset,
  onAdd,
  onDone,
}: PassageComposerProps) {
  const [open, setOpen] = useState(false);
  const [note, setNote] = useState("");
  const [problem, setProblem] = useState("");
  if (selected === "") return null;
  function add() {
    const text = note.trim();
    if (text === "") return;
    const trimmed = selected.trim();
    const lead = selected.length - selected.trimStart().length;
    let start: number;
    if (offset !== null && content.startsWith(trimmed, offset + lead)) {
      start = offset + lead;
    } else {
      const first = content.indexOf(trimmed);
      if (first !== -1 && first !== content.lastIndexOf(trimmed)) {
        setProblem("That passage occurs more than once in the file. Select a longer passage.");
        return;
      }
      start = first;
    }
    const anchor =
      start === -1 ? null : anchorForSelection(path, content, start, start + trimmed.length);
    if (anchor === null) {
      setProblem("That selection is not plain text of this file. Select text from the raw view.");
      return;
    }
    onAdd({ ...anchor, note: text });
    setNote("");
    setProblem("");
    setOpen(false);
    onDone();
  }
  if (!open) {
    return (
      <div className="flex items-center gap-2">
        <Button
          size="sm"
          onClick={() => {
            setOpen(true);
          }}
        >
          <MessageSquarePlus aria-hidden="true" />
          Comment on selection
        </Button>
        <q className="text-fg-muted min-w-0 truncate text-xs">{clipQuote(selected)}</q>
      </div>
    );
  }
  return (
    <div className="flex flex-col gap-2">
      <p className="text-fg-muted text-xs break-words">
        On: <q>{clipQuote(selected)}</q>
      </p>
      <div className="flex items-end gap-2">
        <Field label="Comment on this passage" className="min-w-0 flex-1">
          <Input
            autoFocus
            value={note}
            onChange={(event) => {
              setNote(event.target.value);
            }}
            onKeyDown={(event) => {
              if (event.key !== "Enter") return;
              event.preventDefault();
              add();
            }}
          />
        </Field>
        <Button disabled={note.trim() === ""} onClick={add}>
          Add comment
        </Button>
        <Button
          variant="ghost"
          onClick={() => {
            setOpen(false);
            setProblem("");
            onDone();
          }}
        >
          Cancel
        </Button>
      </div>
      {problem === "" ? null : (
        <p role="alert" className="text-tone-danger text-xs">
          {problem}
        </p>
      )}
    </div>
  );
}
