import { Check, X } from "lucide-react";

import { type StructureCheck, specStructure, ticketStructure } from "@/domain/specSkeleton";
import { cn } from "@/ui/cn";

/** Which file's structure rules apply. */
export type StructureKind = "spec" | "ticket";

export interface StructureChecklistProps {
  readonly kind: StructureKind;
  /** The editor's current text, saved or not. */
  readonly text: string;
  readonly className?: string;
}

function check(kind: StructureKind, text: string): StructureCheck {
  return kind === "spec" ? specStructure(text) : ticketStructure(text);
}

/**
 * The structure the server requires of a spec or a ticket, checked against
 * the text as it is typed: one row per required heading or header line, and
 * what was read from the criteria. A convenience only: Save stays enabled
 * and the server validates the saved text again, so its refusal still shows
 * under the editor.
 */
export function StructureChecklist({ kind, text, className }: StructureChecklistProps) {
  const { rows, passes } = check(kind, text);
  const failing = rows.filter((row) => !row.ok).length;
  return (
    <section
      aria-label="Structure checklist"
      data-testid="structure-checklist"
      className={cn(
        "border-border bg-surface-sunken flex flex-col gap-2 rounded-md border p-3",
        className,
      )}
    >
      <h4 className="text-fg-muted text-xs font-semibold">Structure</h4>
      <ul className="flex flex-col gap-1">
        {rows.map((row) => (
          <li
            key={row.label}
            data-ok={row.ok}
            className="flex items-start gap-1.5 font-mono text-xs"
          >
            {row.ok ? (
              <Check aria-hidden="true" className="text-tone-success mt-0.5 size-3 shrink-0" />
            ) : (
              <X aria-hidden="true" className="text-tone-danger mt-0.5 size-3 shrink-0" />
            )}
            <span className={cn("min-w-0 break-words", row.ok ? "text-fg-muted" : "text-fg")}>
              <span className="sr-only">{row.ok ? "OK: " : "Problem: "}</span>
              {row.label}
              {row.note === "" ? null : (
                <span className={row.ok ? "text-fg-subtle" : "text-tone-danger"}>
                  {` (${row.note})`}
                </span>
              )}
            </span>
          </li>
        ))}
      </ul>
      <p role="status" className="text-fg-subtle text-xs">
        {passes
          ? "Structure is complete. The server checks the saved text again."
          : failing === 0
            ? "The server would refuse this text."
            : `${failing} to fix. The server refuses a save without ${failing === 1 ? "it" : "them"}.`}
      </p>
    </section>
  );
}
