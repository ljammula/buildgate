import type { TimelineRow } from "@/domain/runDetail";
import { StageGlyph } from "@/features/run-detail/StageGlyph";
import { CodeBlock } from "@/ui/CodeBlock";
import { cn } from "@/ui/cn";

/**
 * One stage of the Timeline, a list item of its own. Worker-relayed lines
 * (round and agent notes) are untrusted, display-only text: they stay in the
 * secondary style and render as text only; the factory's own evidence rounds
 * read as body text.
 */
export function TimelineRowItem({ row }: { row: TimelineRow }) {
  return (
    <li data-testid={`timeline-row-${row.rowKey}`} className="flex flex-col gap-0.5 py-1">
      <div className="flex items-center gap-2 text-sm">
        <StageGlyph glyph={row.glyph} />
        <span className="text-fg">{row.label}</span>
        {row.durationText !== null ? (
          <span className="text-xs text-fg-muted tabular-nums">{row.durationText}</span>
        ) : null}
      </div>
      {row.subtitle !== null ? <p className="pl-6 text-xs text-fg-muted">{row.subtitle}</p> : null}
      {row.subRows.map((sub, i) => (
        <div key={i} className="flex items-center gap-1.5 pl-6">
          <StageGlyph glyph={sub.glyph} small />
          <span
            className={cn(
              "min-w-0 flex-1 text-xs",
              sub.factoryAuthored ? "text-fg" : "text-fg-muted",
            )}
          >
            {sub.label}
          </span>
          {sub.durationText !== null ? (
            <span className="text-xs text-fg-muted tabular-nums">{sub.durationText}</span>
          ) : null}
        </div>
      ))}
      {row.noteLines.length > 0 ? (
        <CodeBlock label="Agent notes" wrap className="ml-6 text-[11px]">
          {row.noteLines.join("\n")}
        </CodeBlock>
      ) : null}
    </li>
  );
}
