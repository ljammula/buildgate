import { useState } from "react";

import type { RequestSummary } from "@/domain/request";

import {
  type ContentFile,
  contentFiles,
  editBlockedReason,
  editChangeNotice,
} from "./requestDetailLogic";

/** What one file's editor needs from the page's edit session. */
export interface EditorBinding {
  /** Reports whether the text differs from what the editor opened with. */
  readonly onDirtyChange: (dirty: boolean) => void;
  /** Set while a newer record arrived under the editor and the operator has not answered. */
  readonly notice: string | null;
  readonly onKeepEditing: () => void;
  /** Why Save is off, or null. */
  readonly blockedReason: string | null;
}

export interface EditSession {
  /** The files to show: the request's own, plus the edited one if it left the list while unsaved. */
  readonly files: readonly ContentFile[];
  /** The id of the file being edited, or null. */
  readonly editingId: string | null;
  /** Text is unsaved now (live). */
  readonly dirty: boolean;
  /** Text has been changed since the editor opened and not saved or discarded (latched: reverting the text does not disarm the leave guard mid-flight). */
  readonly armed: boolean;
  readonly start: (id: string) => void;
  readonly stop: () => void;
  readonly binding: EditorBinding;
}

interface Started {
  readonly file: ContentFile;
  readonly state: string;
  readonly content: string;
  /** The `state + content` the operator answered "Keep editing" to. */
  readonly answered: string | null;
}

function contentOf(request: RequestSummary, file: ContentFile): string {
  if (file.ticket === null) return request.spec;
  const index = file.ticket.index;
  return request.tickets.find((t) => t.index === index)?.content ?? file.ticket.content;
}

/**
 * The page's one open editor. An editor with nothing unsaved closes by itself
 * when its file stops being editable (the request moved on), so Approve is not
 * held disabled for an editor nobody needs. An editor WITH unsaved text never
 * closes by itself: a newer record only raises a notice (and Save turns off
 * if the file is no longer editable); the operator chooses Keep editing or
 * Discard. Save still sends the base hash of what the edit started from, so
 * the server's own 409 decides whether a changed file may be overwritten.
 */
export function useEditSession(request: RequestSummary, canWrite: boolean): EditSession {
  const current = contentFiles(request, canWrite);
  const [started, setStarted] = useState<Started | null>(null);
  const [dirty, setDirty] = useState(false);
  const [armed, setArmed] = useState(false);

  const live = started === null ? undefined : current.find((f) => f.id === started.file.id);
  const editable = live?.editable === true;
  if (started !== null && !editable && !dirty) {
    setStarted(null);
    setArmed(false);
  }

  const shown =
    started === null || live !== undefined
      ? current
      : [{ ...started.file, editable: false }, ...current];
  const content = started === null ? "" : contentOf(request, started.file);
  const key = started === null ? "" : `${request.state}\n${content}`;
  const contentChanged = started !== null && content !== started.content;
  const changed =
    started !== null && (request.state !== started.state || contentChanged || !editable);

  const stop = () => {
    setStarted(null);
    setDirty(false);
    setArmed(false);
  };

  return {
    files: shown,
    editingId: started?.file.id ?? null,
    dirty,
    armed,
    start: (id) => {
      const file = current.find((f) => f.id === id);
      if (file === undefined) return;
      setStarted({ file, state: request.state, content: contentOf(request, file), answered: null });
      setDirty(false);
      setArmed(false);
    },
    stop,
    binding: {
      onDirtyChange: (next) => {
        setDirty(next);
        if (next) setArmed(true);
      },
      notice:
        changed && started.answered !== key
          ? editChangeNotice({ state: request.state, contentChanged, editable })
          : null,
      onKeepEditing: () => {
        setStarted((s) => (s === null ? s : { ...s, answered: key }));
      },
      blockedReason: started === null ? null : editBlockedReason(request.state, editable),
    },
  };
}
