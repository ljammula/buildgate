import { CircleAlert, CircleCheck, CircleDashed, CircleMinus, Loader2 } from "lucide-react";

import type { StageGlyph as Glyph } from "@/domain/runDetail";
import { cn } from "@/ui/cn";

const words: Readonly<Record<Glyph, string>> = {
  running: "Running",
  passed: "Passed",
  failed: "Failed",
  skipped: "Skipped",
  pending: "Pending",
};

/**
 * A stage's status as an icon plus its status word, so the outcome is never
 * conveyed by colour alone: the word is the accessible text, the icon is
 * decoration.
 */
export function StageGlyph({ glyph, small = false }: { glyph: Glyph; small?: boolean }) {
  const size = small ? "size-3.5" : "size-4";
  const props = { "aria-hidden": true, className: cn(size, "shrink-0") };
  return (
    <span className="inline-flex items-center">
      {glyph === "running" ? (
        <Loader2 {...props} className={cn(size, "shrink-0 animate-spin text-tone-info")} />
      ) : glyph === "passed" ? (
        <CircleCheck {...props} className={cn(size, "shrink-0 text-tone-success")} />
      ) : glyph === "failed" ? (
        <CircleAlert {...props} className={cn(size, "shrink-0 text-tone-danger")} />
      ) : glyph === "skipped" ? (
        <CircleMinus {...props} className={cn(size, "shrink-0 text-fg-subtle")} />
      ) : (
        <CircleDashed {...props} className={cn(size, "shrink-0 text-fg-subtle")} />
      )}
      <span className="sr-only">{words[glyph]}</span>
    </span>
  );
}
