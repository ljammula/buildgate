import { escapeInvisible } from "@/domain/textEscape";
import { readableBuildLog } from "@/domain/runLogText";
import { cn } from "@/ui/cn";

export interface CodeBlockProps {
  readonly children: string;
  /** Wrap long lines instead of scrolling them sideways. */
  readonly wrap?: boolean;
  /** A Tailwind max-height class, e.g. "max-h-80"; the block scrolls past it. */
  readonly maxHeight?: string;
  readonly label?: string;
  readonly className?: string;
}

/**
 * Monospace text, as text: it is agent-written (a log, a halt reason), so it
 * is never HTML and invisible or bidi characters are written out. The block
 * scrolls and takes focus, so a keyboard user can read a long one.
 */
export function CodeBlock({
  children,
  wrap = false,
  maxHeight,
  label = "Text",
  className,
}: CodeBlockProps) {
  return (
    <pre
      tabIndex={0}
      role="region"
      aria-label={label}
      className={cn(
        "overflow-auto rounded-md border border-border bg-surface-sunken p-2 font-mono text-xs",
        wrap ? "break-words whitespace-pre-wrap" : "whitespace-pre",
        maxHeight,
        className,
      )}
    >
      {escapeInvisible(children)}
    </pre>
  );
}

export interface LogViewProps {
  readonly text: string;
}

/** A run's build log: progress protocol lines made readable, the rest verbatim. */
export function LogView({ text }: LogViewProps) {
  return (
    <CodeBlock label="Build log" maxHeight="max-h-80">
      {text === "" ? "(no log output yet)" : readableBuildLog(text)}
    </CodeBlock>
  );
}
