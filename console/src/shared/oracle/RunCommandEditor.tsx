import { useState } from "react";

import { ApiError } from "@/domain/apiError";
import { escapeInvisible } from "@/domain/textEscape";
import { EscapedText } from "@/shared/oracle/EscapedText";
import { Button } from "@/ui/Button";
import { describeError } from "@/ui/ErrorDisplay";
import { Textarea } from "@/ui/Input";

/**
 * The text field's starting value. Invalid bytes cannot round-trip through a
 * text field: they show as U+FFFD here, and the saved file will differ from
 * what was displayed.
 */
function editableRunCommandText(shown: string): string {
  return Array.from(shown, (ch) => {
    const cp = ch.codePointAt(0) ?? 0;
    return cp >= 0xd800 && cp <= 0xdfff ? "�" : ch;
  }).join("");
}

export interface RunCommandEditorProps {
  /** The text shown for the file when Edit was pressed (surrogate escapes allowed). */
  readonly initialText: string;
  readonly saving: boolean;
  /** The last save's failure, shown inline; the editor stays open. */
  readonly saveError: unknown;
  readonly onSave: (content: string) => void;
  readonly onCancel: () => void;
  /** The oracle draft's proposed command; when it differs from the field, "Use suggestion" puts it there (saving is still the operator's act). */
  readonly suggestion?: string;
}

/** The RUN_COMMAND.txt editor: a field, a preview of invisible characters, Save and Cancel. */
export function RunCommandEditor({
  initialText,
  saving,
  saveError,
  onSave,
  onCancel,
  suggestion = "",
}: RunCommandEditorProps) {
  const [text, setText] = useState(() => editableRunCommandText(initialText));
  const hasHidden = escapeInvisible(text) !== text;
  const errorText =
    saveError instanceof ApiError ? saveError.serverMessage : describeError(saveError).raw;
  return (
    <div className="flex w-full flex-col gap-1">
      <Textarea
        mono
        rows={4}
        aria-label="RUN_COMMAND.txt content"
        value={text}
        onChange={(event) => {
          setText(event.target.value);
        }}
      />
      {hasHidden ? (
        <div data-testid="oracle-run-command-preview">
          <EscapedText
            text={`Contains invisible characters; as saved: ${text}`}
            className="text-tone-danger font-mono text-xs"
          />
        </div>
      ) : null}
      {saveError !== null && saveError !== undefined ? (
        <div data-testid="oracle-run-command-error">
          <EscapedText text={`Could not save: ${errorText}`} className="text-tone-danger text-sm" />
        </div>
      ) : null}
      <div className="flex justify-end gap-2">
        {suggestion !== "" && suggestion !== text ? (
          <Button
            size="sm"
            variant="ghost"
            className="mr-auto"
            disabled={saving}
            onClick={() => {
              setText(suggestion);
            }}
          >
            Use suggestion
          </Button>
        ) : null}
        <Button size="sm" variant="ghost" disabled={saving} onClick={onCancel}>
          Cancel
        </Button>
        <Button
          size="sm"
          variant="primary"
          disabled={saving}
          onClick={() => {
            onSave(text);
          }}
        >
          {saving ? "Saving..." : "Save"}
        </Button>
      </div>
    </div>
  );
}
