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

import { CriteriaListEditor } from "./CriteriaListEditor";
import { SectionEditor } from "./SectionEditor";
import { StructureChecklist, type StructureKind } from "./StructureChecklist";
import { TicketFieldsEditor } from "./TicketFieldsEditor";
import { TicketListEditor } from "./TicketListEditor";
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
  /** For a ticket: the spec's numbered criteria, offered as what the ticket may cover. */
  readonly specCriteria?: readonly string[];
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
 * as it is typed; it never blocks Save. The fields above the text (a spec's
 * criteria list, a ticket's header lines and covered criteria) are other
 * ways to write the same text: they change `text` and nothing else, so the
 * save, its base hash and the server's validation are those of a hand edit,
 * and the line diff under the text shows everything that will be sent.
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
  specCriteria,
}: FileEditorProps) {
  const [opened] = useState(initialContent);
  const [text, setText] = useState(initialContent);
  const [confirmingDiscard, setConfirmingDiscard] = useState(false);
  // A section field open with text not applied: Save would leave it behind.
  const [sectionPending, setSectionPending] = useState(false);
  const field = useRef<HTMLTextAreaElement>(null);
  // An open section field counts: closing the editor would drop its text.
  const dirty = text !== opened || sectionPending;
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
      if (!saving && blockedReason === null && !sectionPending) void save();
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
      {structure === undefined ? null : (
        <div className="border-border flex flex-col gap-2 rounded-md border p-3">
          {structure === "spec" ? (
            <CriteriaListEditor text={text} onChange={setText} disabled={blockedReason !== null} />
          ) : (
            <>
              <TicketFieldsEditor
                text={text}
                onChange={setText}
                specCriteria={specCriteria ?? []}
                disabled={blockedReason !== null}
              />
              <TicketListEditor
                text={text}
                heading="### Steps"
                onChange={setText}
                disabled={blockedReason !== null}
              />
              <TicketListEditor
                text={text}
                heading="### Files to touch"
                onChange={setText}
                disabled={blockedReason !== null}
              />
            </>
          )}
          <SectionEditor
            kind={structure}
            text={text}
            onChange={setText}
            onPendingChange={setSectionPending}
            disabled={blockedReason !== null}
          />
          <p className="text-fg-subtle text-xs">
            These fields rewrite the text below, which is exactly what Save sends.
          </p>
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
      {structure === undefined || !dirty ? null : (
        <section
          aria-label="Your changes"
          data-testid="edit-changes"
          className="flex flex-col gap-1"
        >
          <h4 className="text-fg-muted text-xs font-semibold">Your changes</h4>
          <div className="max-h-60 overflow-auto">
            <TextDiffView before={opened} after={text} beforeLabel="opened" afterLabel="to save" />
          </div>
        </section>
      )}
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
      {!sectionPending ? null : (
        <p data-testid="edit-section-pending" className="text-fg-muted text-right text-sm">
          A section is open: Apply or Cancel it before saving.
        </p>
      )}
      <div className="flex justify-end gap-2">
        {blockedReason === null ? null : <CopyButton text={text} label="Copy my text" />}
        <Button variant="ghost" disabled={saving} onClick={requestClose}>
          Cancel
        </Button>
        <Button
          variant="primary"
          disabled={saving || blockedReason !== null || sectionPending}
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
