import { useState } from "react";

import { usePutOracleRunCommand } from "@/api/requestQueries";

export interface RunCommandEditing {
  readonly editing: boolean;
  readonly saving: boolean;
  /** The last save's failure; null once editing starts, is cancelled or a save is retried. */
  readonly saveError: unknown;
  readonly start: () => void;
  readonly cancel: () => void;
  /** Saves `content`; `onSaved` runs once the server has it (the editor is closed by then). */
  readonly save: (content: string, onSaved: () => void) => void;
}

/** The RUN_COMMAND.txt edit session of one request's oracle: open, saving and the last failure. */
export function useRunCommandEditing(id: string): RunCommandEditing {
  const put = usePutOracleRunCommand(id);
  const [editing, setEditing] = useState(false);
  const [saveError, setSaveError] = useState<unknown>(null);
  return {
    editing,
    saving: put.isPending,
    saveError,
    start: () => {
      setSaveError(null);
      setEditing(true);
    },
    cancel: () => {
      setEditing(false);
      setSaveError(null);
    },
    save: (content, onSaved) => {
      setSaveError(null);
      put.mutate(
        { content },
        {
          onSuccess: () => {
            setEditing(false);
            onSaved();
          },
          onError: setSaveError,
        },
      );
    },
  };
}
