import { middleTruncate } from "@/domain/middleTruncate";
import { cn } from "@/ui/cn";
import { CopyButton } from "@/ui/CopyButton";

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
  writeText,
  className,
}: CompactIdProps) {
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
        <CopyButton size="sm" text={value} label={`Copy ${label}`} writeText={writeText} />
      ) : null}
    </span>
  );
}
