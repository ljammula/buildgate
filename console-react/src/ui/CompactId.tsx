import { Check, Copy } from "lucide-react";
import { useEffect, useRef, useState } from "react";

import { cn } from "@/ui/cn";
import { middleTruncate } from "@/ui/middleTruncate";

export interface CompactIdProps {
  /** The full id, SHA or path. */
  readonly value: string;
  /** The longest the visible text gets; longer values lose their middle. */
  readonly max?: number;
  /** What the copy button names, e.g. "request id". Omit the button with `copy={false}`. */
  readonly label?: string;
  readonly copy?: boolean;
  readonly writeText?: (text: string) => Promise<void>;
  readonly className?: string;
}

const COPIED_MS = 2000;

function defaultWriteText(text: string): Promise<void> {
  return navigator.clipboard.writeText(text);
}

/**
 * An id that never wraps mid-token: shown with its middle cut out, the whole
 * value in the tooltip and in the accessibility tree, and one click from the
 * clipboard. A value that already fits renders as plain text.
 */
export function CompactId({
  value,
  max = 28,
  label = "id",
  copy = true,
  writeText = defaultWriteText,
  className,
}: CompactIdProps) {
  const [copied, setCopied] = useState(false);
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  useEffect(
    () => () => {
      clearTimeout(timer.current);
    },
    [],
  );

  async function copyValue() {
    try {
      await writeText(value);
    } catch {
      return;
    }
    setCopied(true);
    clearTimeout(timer.current);
    timer.current = setTimeout(() => {
      setCopied(false);
    }, COPIED_MS);
  }

  const short = middleTruncate(value, max);
  const truncated = short !== value;
  return (
    <span className={cn("inline-flex min-w-0 max-w-full items-center gap-1 font-mono", className)}>
      <span title={value} className="whitespace-nowrap">
        {truncated ? (
          <>
            <span aria-hidden="true">{short}</span>
            <span className="sr-only">{value}</span>
          </>
        ) : (
          value
        )}
      </span>
      {copy && value !== "" ? (
        <button
          type="button"
          aria-label={copied ? "Copied" : `Copy ${label}`}
          title={copied ? "Copied" : `Copy ${label}`}
          onClick={() => void copyValue()}
          className="text-fg-subtle hover:bg-surface-hover hover:text-fg focus-visible:outline-ring inline-grid size-5 shrink-0 place-items-center rounded"
        >
          {copied ? (
            <Check aria-hidden="true" className="size-3" />
          ) : (
            <Copy aria-hidden="true" className="size-3" />
          )}
        </button>
      ) : null}
    </span>
  );
}
