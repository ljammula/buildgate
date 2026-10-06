import { useEffect } from "react";

import { isTypingTarget } from "./triageModel";

export interface TriageKeyActions {
  readonly move: (delta: number) => void;
  readonly approve: () => void;
  readonly reject: () => void;
}

/**
 * j/k move, a approves, r rejects. Ignored while the operator types in a
 * field, with a modifier held, or while `suspended` (a dialog is open); a
 * held key never repeats an approve or reject.
 */
export function useTriageKeys(actions: TriageKeyActions, suspended: boolean): void {
  const { move, approve, reject } = actions;
  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent): void => {
      if (suspended || event.defaultPrevented) return;
      if (event.ctrlKey || event.metaKey || event.altKey) return;
      if (isTypingTarget(event.target)) return;
      switch (event.key) {
        case "j":
          move(1);
          break;
        case "k":
          move(-1);
          break;
        case "a":
          if (!event.repeat) approve();
          break;
        case "r":
          if (!event.repeat) reject();
          break;
        default:
          return;
      }
      event.preventDefault();
    };
    window.addEventListener("keydown", onKeyDown);
    return () => {
      window.removeEventListener("keydown", onKeyDown);
    };
  }, [move, approve, reject, suspended]);
}
