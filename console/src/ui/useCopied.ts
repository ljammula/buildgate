import { useCallback, useEffect, useRef, useState } from "react";

/** How long "Copied" stays up before the control goes back to its idle state. */
export const copiedResetMs = 2000;

function defaultWriteText(text: string): Promise<void> {
  return navigator.clipboard.writeText(text);
}

export interface UseCopied {
  /** True for `copiedResetMs` after a successful write. */
  readonly copied: boolean;
  /** Writes `text`; a refused write leaves `copied` false and never throws. */
  readonly copy: (text: string) => Promise<void>;
}

/**
 * Clipboard copy with a transient "copied" flag. `writeText` is injectable so
 * a test never touches the real clipboard. A second copy restarts the reset
 * timer, and the timer is cleared on unmount, so nothing fires afterwards.
 */
export function useCopied(
  writeText: (text: string) => Promise<void> = defaultWriteText,
): UseCopied {
  const [copied, setCopied] = useState(false);
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  const mounted = useRef(false);
  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
      clearTimeout(timer.current);
    };
  }, []);

  const copy = useCallback(
    async (text: string) => {
      try {
        await writeText(text);
      } catch {
        return;
      }
      // The write can settle after the control is gone: no state, no timer.
      if (!mounted.current) return;
      setCopied(true);
      clearTimeout(timer.current);
      timer.current = setTimeout(() => {
        setCopied(false);
      }, copiedResetMs);
    },
    [writeText],
  );
  return { copied, copy };
}
