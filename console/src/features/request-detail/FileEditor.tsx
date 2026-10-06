import { type KeyboardEvent, useEffect, useRef, useState } from "react";

import { ApiError } from "@/domain/apiError";
import { sha256Hex } from "@/domain/contentHash";
import { Button } from "@/ui/Button";
import { ConfirmDialog } from "@/ui/ConfirmDialog";
import { CopyButton } from "@/ui/CopyButton";
import { TextDiffView } from "@/ui/DiffView";
import { Textarea } from "@/ui/Input";
import { describeError } from "@/ui/ErrorDisplay";
import { Spinner } from "@/ui/Feedback";

import { StructureChecklist, type StructureKind } from "./StructureChecklist";
import type { EditorBinding } from "./useEditSession";

export interface FileEditorProps {
  /** The file's request-relative path; names the editor. */
  readonly path: string;
  /** The content shown when the operator pressed Edit. Its hash is the save's base. */
  readonly initialContent: string;
  /** Saves via the PUT route; rejects with the server's error (422, 409 with `current_sha256`, ...). */
  readonly onSave: (content: string, baseSha256: string) => Promise<void>;
  /** The server's current content, fetched after a 409 to diff against the operator's text. */
  readonly onFetchCurrent: () => Promise<string>;
  /** Leaves the editor (saved, cancelled, or the operator discarded their edit). */
  readonly onClose: () => void;
  /** Newer-record notice, dirty reporting and the save lock; see `useEditSession`. */
  readonly session?: EditorBinding;
  /** Shows the live structure checklist for this kind of file beside the text. */
  readonly structure?: StructureKind;
}

/**
 * Edit in place: a monospace textarea pre-filled with the current content,
 * Save and Cancel. Save sends `base_sha256` = the hash of the content the
 * editor was OPENED with, so the server refuses (409) a change made
 * elsewhere meanwhile instead of overwriting it. On a 409 the operator's
 * text is kept, what is on disk now is fetched and diffed against it, and
 * the operator either discards their edit or keeps it re-based onto the
 * reported hash (an informed overwrite of what they just saw). Any other
 * failure (a 422's validation message) shows verbatim and keeps the edit.
 * With `structure`, a checklist of what that 422 would name follows the text
 * as it is typed; it never blocks Save.
 * Keyboard: the text is focused on open, Cmd/Ctrl+S saves, Esc cancels (asking
 * first when there is unsaved text). A newer record arriving never discards
 * the text: see `session`.
 */
export function FileEditor({
  path,
  initialContent,
  onSave,
  onFetchCurrent,
  onClose,
  session,
  structure,
}: FileEditorProps) {
  const [opened] = useState(initialContent);
  const [text, setText] = useState(initialContent);
  const [confirmingDiscard, setConfirmingDiscard] = useState(false);
  const field = useRef<HTMLTextAreaElement>(null);
  const dirty = text !== opened;
  const blockedReason = session?.blockedReason ?? null;
  const onDirtyChange = session?.onDirtyChange;
  useEffect(() => {
    onDirtyChange?.(dirty);
  }, [dirty, onDirtyChange]);
  useEffect(() => {
    const el = field.current;
    if (el === null) return;
    el.focus();
    el.setSelectionRange(0, 0);
  }, []);
  const [baseSha256, setBaseSha256] = useState(() => sha256Hex(initialContent));
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<unknown>(null);
  const [currentText, setCurrentText] = useState<string | null>(null);
  const [loadingConflict, setLoadingConflict] = useState(false);
  const [conflictFetchError, setConflictFetchError] = useState<unknown>(null);

  const conflict = saveError instanceof ApiError && saveError.status === 409 ? saveError : null;

  async function loadConflictDiff() {
    setLoadingConflict(true);
    setConflictFetchError(null);
    try {
      setCurrentText(await onFetchCurrent());
    } catch (error) {
      setConflictFetchError(error);
    } finally {
      setLoadingConflict(false);
    }
  }

  function requestClose() {
    if (dirty) setConfirmingDiscard(true);
    else onClose();
  }

  function onKeyDown(event: KeyboardEvent<HTMLTextAreaElement>) {
    if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "s") {
      event.preventDefault();
      if (!saving && blockedReason === null) void save();
    } else if (event.key === "Escape") {
      event.preventDefault();
      requestClose();
    }
  }

  async function save() {
    setSaving(true);
    setSaveError(null);
    try {
      await onSave(text, baseSha256);
      onClose();
      return;
    } catch (error) {
      setSaveError(error);
      if (error instanceof ApiError && error.status === 409) void loadConflictDiff();
    } finally {
      setSaving(false);
    }
  }

  function keepEditingRebased() {
    const next = conflict?.currentSha256 ?? null;
    if (next === null) return;
    setBaseSha256(next);
    setSaveError(null);
    setCurrentText(null);
    setConflictFetchError(null);
  }

  return (
    <div className="flex flex-col gap-2">
      {session?.notice == null ? null : (
        <div
          role="alert"
          data-testid="edit-changed-notice"
          className="bg-tone-warning-soft border-tone-warning-border flex flex-col gap-2 rounded-md border p-3 text-sm"
        >
          <p>{session.notice}</p>
          <div className="flex flex-wrap gap-2">
            <Button variant="primary" onClick={session.onKeepEditing}>
              Keep editing
            </Button>
            <Button onClick={onClose}>Discard my changes</Button>
          </div>
        </div>
      )}
      <div className="flex flex-col gap-2 lg:flex-row lg:items-start">
        <Textarea
          ref={field}
          mono
          readOnly={blockedReason !== null}
          aria-label={`Edit ${path}`}
          rows={structure === undefined ? 14 : 22}
          className="min-w-0 lg:flex-1"
          value={text}
          onChange={(event) => {
            setText(event.target.value);
          }}
          onKeyDown={onKeyDown}
        />
        {structure === undefined ? null : (
          <StructureChecklist kind={structure} text={text} className="lg:w-72 lg:shrink-0" />
        )}
      </div>
      {blockedReason === null ? null : (
        <p data-testid="edit-blocked" className="text-fg-muted text-sm">
          {blockedReason}
        </p>
      )}
      {conflict !== null ? (
        <div
          role="alert"
          data-testid="edit-conflict"
          className="bg-tone-danger-soft border-tone-danger-border flex flex-col gap-2 rounded-md border p-3 text-sm"
        >
          <p className="text-tone-danger">
            {`This file changed on disk since you started editing (current hash: ${
              conflict.currentSha256 ?? "unknown"
            }). Your edit above is unchanged.`}
          </p>
          {loadingConflict ? <Spinner label="Loading the current content" /> : null}
          {conflictFetchError === null ? null : (
            <p className="text-tone-danger">
              {`Could not load the current content to diff: ${describeError(conflictFetchError).raw}`}
            </p>
          )}
          {currentText === null ? null : (
            <div data-testid="edit-conflict-diff" className="max-h-60 overflow-auto">
              <TextDiffView
                before={currentText}
                after={text}
                beforeLabel="current (on disk)"
                afterLabel="yours (unsaved)"
              />
            </div>
          )}
          <div className="flex flex-wrap gap-2">
            <Button onClick={onClose}>Discard mine and reload</Button>
            <Button
              variant="primary"
              disabled={conflict.currentSha256 === null}
              onClick={keepEditingRebased}
            >
              Keep editing (base = current)
            </Button>
          </div>
        </div>
      ) : saveError === null ? null : (
        <p role="alert" className="text-tone-danger text-sm">
          {`Could not save: ${describeError(saveError).raw}`}
        </p>
      )}
      <div className="flex justify-end gap-2">
        {blockedReason === null ? null : <CopyButton text={text} label="Copy my text" />}
        <Button variant="ghost" disabled={saving} onClick={requestClose}>
          Cancel
        </Button>
        <Button
          variant="primary"
          disabled={saving || blockedReason !== null}
          onClick={() => void save()}
        >
          {saving ? "Saving..." : "Save"}
        </Button>
      </div>
      <ConfirmDialog
        open={confirmingDiscard}
        onOpenChange={setConfirmingDiscard}
        title="Discard your changes?"
        confirmLabel="Discard changes"
        cancelLabel="Keep editing"
        tone="danger"
        onConfirm={onClose}
      >
        {`Your edit to ${path} has not been saved.`}
      </ConfirmDialog>
    </div>
  );
}
