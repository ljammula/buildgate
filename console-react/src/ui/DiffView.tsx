import { escapeInvisible } from "@/domain/textEscape";
import { unifiedLineDiff } from "@/domain/textDiff";
import { cn } from "@/ui/cn";

export type DiffLineKind = "add" | "del" | "hunk" | "meta" | "context";

/**
 * A line's kind from its first characters. `+++` and `---` are file
 * headers, not additions or deletions, and stay uncoloured ("meta").
 */
export function classifyDiffLine(line: string): DiffLineKind {
  if (line.startsWith("+++") || line.startsWith("---")) return "meta";
  if (line.startsWith("+")) return "add";
  if (line.startsWith("-")) return "del";
  if (line.startsWith("@@")) return "hunk";
  return "context";
}

const kindClass: Record<DiffLineKind, string> = {
  add: "bg-diff-add text-diff-add-fg",
  del: "bg-diff-del text-diff-del-fg",
  hunk: "bg-diff-hunk text-fg-muted",
  meta: "text-fg-muted",
  context: "",
};

export interface DiffViewProps {
  readonly diff: string;
  /** The server cut the diff at its storage cap. */
  readonly truncated?: boolean;
  readonly className?: string;
}

/**
 * A unified diff, one text line per row, coloured by first character. The
 * text is agent-written: lines are text nodes, never HTML, and invisible or
 * bidi characters are written out so a line cannot hide what it contains.
 */
export function DiffView({ diff, truncated = false, className }: DiffViewProps) {
  if (diff === "") return <p className="text-sm text-fg-muted">No changes.</p>;
  const lines = diff.split("\n");
  // A diff ends in a newline; the empty tail is not a line.
  if (lines[lines.length - 1] === "") lines.pop();
  return (
    <div className={className}>
      {truncated ? (
        <p
          role="status"
          className="rounded-t-md border border-tone-danger-border bg-tone-danger-soft p-2 text-sm text-tone-danger"
        >
          This diff was too large and has been truncated.
        </p>
      ) : null}
      <pre
        tabIndex={0}
        role="region"
        aria-label="Diff"
        className="overflow-auto rounded-md border border-border bg-surface-sunken py-2 font-mono text-xs"
      >
        {lines.map((line, i) => {
          const kind = classifyDiffLine(line);
          return (
            <div
              key={i}
              data-line={kind}
              className={cn("min-h-4 min-w-max px-2 whitespace-pre", kindClass[kind])}
            >
              {escapeInvisible(line) || "​"}
            </div>
          );
        })}
      </pre>
    </div>
  );
}

export interface TextDiffViewProps {
  readonly before: string;
  readonly after: string;
  /** Written above the diff as its `---` / `+++` file headers. */
  readonly beforeLabel?: string;
  readonly afterLabel?: string;
  readonly className?: string;
}

/** A line diff of two texts (a revision against the current file), in the same view as a run's diff. */
export function TextDiffView({
  before,
  after,
  beforeLabel = "before",
  afterLabel = "after",
  className,
}: TextDiffViewProps) {
  const diff = `--- ${beforeLabel}\n+++ ${afterLabel}\n${unifiedLineDiff(before, after)}`;
  return <DiffView diff={diff} {...(className === undefined ? {} : { className })} />;
}
