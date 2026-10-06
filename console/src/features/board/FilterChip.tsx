import type { ReactNode } from "react";

import { cn } from "@/ui/cn";

export interface FilterChipProps {
  readonly pressed: boolean;
  readonly onPressedChange: () => void;
  readonly children: ReactNode;
}

/** A toggle pill for one filter value. */
export function FilterChip({ pressed, onPressedChange, children }: FilterChipProps) {
  return (
    <button
      type="button"
      aria-pressed={pressed}
      onClick={onPressedChange}
      className={cn(
        "h-7 rounded-full border px-3 text-xs font-medium transition-colors",
        pressed
          ? "border-accent bg-accent-soft text-accent"
          : "border-border bg-surface-raised text-fg-muted hover:bg-surface-hover hover:text-fg",
      )}
    >
      {children}
    </button>
  );
}
