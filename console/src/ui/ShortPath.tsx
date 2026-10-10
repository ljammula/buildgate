import { shortPath } from "@/domain/middleTruncate";
import { cn } from "@/ui/cn";
import { CopyButton } from "@/ui/CopyButton";

export interface ShortPathProps {
  readonly path: string;
  /** How many trailing segments to keep; the rest shows as a leading "…/". */
  readonly keep?: number;
  /** What the copy button names, e.g. "workspace path"; the button is drawn only with a label. */
  readonly copyLabel?: string;
  readonly writeText?: (text: string) => Promise<void>;
  readonly className?: string;
}

/**
 * A long absolute path as its last segments, in monospace, with the whole
 * path in the tooltip and, with `copyLabel`, one click from the clipboard:
 * the end of a path says which project, the front only says whose home
 * directory. The one place a path is shortened.
 */
export function ShortPath({ path, keep = 2, copyLabel, writeText, className }: ShortPathProps) {
  const text = (
    <span title={path} className={cn("block truncate font-mono text-xs", className)}>
      {shortPath(path, keep)}
    </span>
  );
  if (copyLabel === undefined || path === "") return text;
  return (
    <span className="inline-flex min-w-0 max-w-full items-center gap-1">
      {text}
      <CopyButton size="sm" text={path} label={`Copy ${copyLabel}`} writeText={writeText} />
    </span>
  );
}
