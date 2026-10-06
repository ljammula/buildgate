import { Pencil } from "lucide-react";

import type { OracleFileContent } from "@/domain/oracle";
import { OracleContentBox } from "@/shared/oracle/OracleContentBox";
import { RunCommandEditor } from "@/shared/oracle/RunCommandEditor";
import type { RunCommandEditing } from "@/shared/oracle/useRunCommandEditing";
import { Button } from "@/ui/Button";

export interface RunCommandBlockProps {
  readonly keyId: string;
  readonly content: OracleFileContent;
  readonly editing: RunCommandEditing;
  readonly canWrite: boolean;
  /** The draft's proposed command, offered as "Use suggestion" in the editor. */
  readonly suggestion: string;
  /** Runs after a save succeeded. */
  readonly onSaved: () => void;
}

/** The body of the RUN_COMMAND.txt tile at oracle_review: its text and Edit, or the editor. */
export function RunCommandBlock({
  keyId,
  content,
  editing,
  canWrite,
  suggestion,
  onSaved,
}: RunCommandBlockProps) {
  if (editing.editing) {
    return (
      <RunCommandEditor
        initialText={content.text}
        saving={editing.saving}
        saveError={editing.saveError}
        suggestion={suggestion}
        onSave={(text) => {
          editing.save(text, onSaved);
        }}
        onCancel={editing.cancel}
      />
    );
  }
  return (
    <>
      <OracleContentBox keyId={keyId} content={content} />
      {canWrite ? (
        <Button size="sm" variant="ghost" className="self-end" onClick={editing.start}>
          <Pencil aria-hidden="true" />
          Edit
        </Button>
      ) : null}
    </>
  );
}
