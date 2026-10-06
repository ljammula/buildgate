import { useState } from "react";

import { ApiError } from "@/domain/apiError";
import { sha256Hex } from "@/domain/contentHash";
import { Button } from "@/ui/Button";
import { TextDiffView } from "@/ui/DiffView";
import { Textarea } from "@/ui/Input";
import { Spinner } from "@/ui/Feedback";

import { errorText } from "./requestDetailLogic";

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
 */
export function FileEditor({
  path,
  initialContent,
  onSave,
  onFetchCurrent,
  onClose,
}: FileEditorProps) {
  const [text, setText] = useState(initialContent);
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
      <Textarea
        mono
        aria-label={`Edit ${path}`}
        rows={14}
        value={text}
        onChange={(event) => {
          setText(event.target.value);
        }}
      />
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
              {`Could not load the current content to diff: ${errorText(conflictFetchError)}`}
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
          {`Could not save: ${errorText(saveError)}`}
        </p>
      )}
      <div className="flex justify-end gap-2">
        <Button variant="ghost" disabled={saving} onClick={onClose}>
          Cancel
        </Button>
        <Button variant="primary" disabled={saving} onClick={() => void save()}>
          {saving ? "Saving..." : "Save"}
        </Button>
      </div>
    </div>
  );
}
