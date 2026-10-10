import { Loader2 } from "lucide-react";

import type { StatusIcon } from "@/domain/status";
import type { StageGlyph as Glyph } from "@/domain/runDetail";
import { cn } from "@/ui/cn";
import { statusIcons } from "@/ui/statusIcons";

const words: Readonly<Record<Glyph, string>> = {
  running: "Running",
  passed: "Passed",
  failed: "Failed",
  skipped: "Skipped",
  pending: "Pending",
};

interface Drawing {
  readonly icon: StatusIcon;
  readonly tone: string;
}

const drawings: Readonly<Record<Exclude<Glyph, "running">, Drawing>> = {
  passed: { icon: "circle_check", tone: "text-tone-success" },
  failed: { icon: "circle_x", tone: "text-tone-danger" },
  skipped: { icon: "circle_minus", tone: "text-fg-subtle" },
  pending: { icon: "circle_dashed", tone: "text-fg-subtle" },
};

export interface StageGlyphProps {
  readonly glyph: Glyph;
  readonly small?: boolean;
  /**
   * The run is making progress and its feed is connected. Only then does a
   * running stage spin: otherwise it is a static icon, so a spinner never
   * shows work that is not happening.
   */
  readonly live: boolean;
  /** The run is stalled: a running stage that is not live reads as a warning, not as in progress. */
  readonly stalled?: boolean;
}

/**
 * A stage's status as an icon plus its status word, so the outcome is never
 * conveyed by colour alone: the word is the accessible text, the icon is
 * decoration.
 */
export function StageGlyph({ glyph, small = false, live, stalled = false }: StageGlyphProps) {
  const size = small ? "size-3.5" : "size-4";
  const drawing: Drawing | null =
    glyph === "running"
      ? live
        ? null
        : stalled
          ? { icon: "triangle_alert", tone: "text-tone-danger" }
          : { icon: "circle_dot", tone: "text-tone-info" }
      : drawings[glyph];
  const Icon = drawing === null ? null : statusIcons[drawing.icon];
  return (
    <span className="inline-flex items-center">
      {Icon === null || drawing === null ? (
        <Loader2
          aria-hidden="true"
          className={cn(size, "shrink-0 animate-spin text-tone-info motion-reduce:animate-none")}
        />
      ) : (
        <Icon aria-hidden="true" className={cn(size, "shrink-0", drawing.tone)} />
      )}
      <span className="sr-only">{words[glyph]}</span>
    </span>
  );
}
