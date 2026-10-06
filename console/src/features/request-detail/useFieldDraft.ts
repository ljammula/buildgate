import { useState } from "react";

export interface FieldDraft {
  readonly value: string;
  readonly onChange: (next: string) => void;
  readonly onBlur: () => void;
}

/**
 * A field whose value is read back out of a document it also writes to.
 * While the operator types, the field shows what they typed (reading back
 * would drop a trailing space or an emptied line mid-keystroke); every
 * change is still written through, and on blur the field shows the
 * document's own reading again.
 */
export function useFieldDraft(fromDocument: string, write: (next: string) => void): FieldDraft {
  const [draft, setDraft] = useState<string | null>(null);
  return {
    value: draft ?? fromDocument,
    onChange: (next) => {
      setDraft(next);
      write(next);
    },
    onBlur: () => {
      setDraft(null);
    },
  };
}
