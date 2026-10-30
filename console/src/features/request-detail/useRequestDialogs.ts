import { useCallback, useRef, useState } from "react";

import type { RequestSummary } from "@/domain/request";

export type DialogKind =
  "approve" | "reject" | "retry" | "cancel" | "sendBack" | "resumeRound" | "resumeScratch";

interface ActiveDialog {
  readonly kind: DialogKind;
  /**
   * The request exactly as the page showed it when the operator pressed the
   * button. Request changes and Send back work from this record and name its
   * stage, whatever arrives while the name prompt or the dialog is up.
   */
  readonly record: RequestSummary;
}

export interface RequestDialogs {
  readonly active: ActiveDialog | null;
  /** A dialog is open: every other action waits. */
  readonly acting: boolean;
  readonly open: (kind: Exclude<DialogKind, "approve">) => void;
  /** spec_review / plan_review approval. */
  readonly openApprove: () => void;
  readonly onOpenChange: (open: boolean) => void;
  readonly onDone: () => void;
  /** The read-back questions whose answer box holds text not yet saved, in id order. */
  readonly unsavedAnswers: readonly string[];
  /** The read-back panel's report of one answer box (stable across renders). */
  readonly onAnswerDraftChanged: (questionId: string, unsaved: boolean) => void;
}

/** Which approval flow dialog is open. */
export function useRequestDialogs(request: RequestSummary): RequestDialogs {
  const [active, setActive] = useState<ActiveDialog | null>(null);
  const [unsaved, setUnsaved] = useState<ReadonlySet<string>>(new Set());
  const finished = useRef(false);
  const onAnswerDraftChanged = useCallback((questionId: string, isUnsaved: boolean) => {
    setUnsaved((current) => {
      if (current.has(questionId) === isUnsaved) return current;
      const next = new Set(current);
      if (isUnsaved) next.add(questionId);
      else next.delete(questionId);
      return next;
    });
  }, []);

  function openApprove() {
    finished.current = false;
    setActive({ kind: "approve", record: request });
  }

  return {
    active,
    acting: active !== null,
    open: (kind) => {
      finished.current = false;
      setActive({ kind, record: request });
    },
    openApprove,
    onOpenChange: (open) => {
      if (open) return;
      setActive(null);
    },
    onDone: () => {
      finished.current = true;
    },
    unsavedAnswers: [...unsaved].sort(),
    onAnswerDraftChanged,
  };
}
