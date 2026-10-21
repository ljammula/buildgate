import { useSyncExternalStore } from "react";

import type { RejectionAnchor, RequestSummary } from "@/domain/request";
import {
  addPendingComment,
  clearPendingComments,
  freshPendingComments,
  getPendingComments,
  pendingKey,
  removePendingComment,
  subscribePendingComments,
} from "@/platform/pendingComments";

export interface PendingComments {
  readonly comments: readonly RejectionAnchor[];
  readonly add: (comment: RejectionAnchor) => void;
  readonly remove: (index: number) => void;
  readonly clear: () => void;
  /** The batch as storage holds it now (another tab may have changed it). */
  readonly fresh: () => readonly RejectionAnchor[];
}

/**
 * The comments written on this request's passages in the stage it is in now,
 * not yet sent. Every component asking for the same request and stage sees
 * the same batch.
 */
export function usePendingComments(
  request: Pick<RequestSummary, "id" | "enteredAt">,
): PendingComments {
  const key = pendingKey(request.id, request.enteredAt);
  const comments = useSyncExternalStore(subscribePendingComments, () => getPendingComments(key));
  return {
    comments,
    add: (comment) => {
      addPendingComment(key, comment);
    },
    remove: (index) => {
      removePendingComment(key, index);
    },
    clear: () => {
      clearPendingComments(key);
    },
    fresh: () => freshPendingComments(key),
  };
}
