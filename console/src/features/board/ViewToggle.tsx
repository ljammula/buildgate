import { Columns3, List } from "lucide-react";

import type { BoardView } from "@/platform/boardPrefs";
import { cn } from "@/ui/cn";

export interface ViewToggleProps {
  readonly view: BoardView;
  readonly onChange: (view: BoardView) => void;
}

const options = [
  { view: "board", label: "Board", icon: Columns3 },
  { view: "list", label: "List", icon: List },
] as const;

/** Board or List: two buttons, the one in use pressed. */
export function ViewToggle({ view, onChange }: ViewToggleProps) {
  return (
    <div
      role="group"
      aria-label="View"
      className="border-border inline-flex h-8 overflow-hidden rounded-md border"
    >
      {options.map(({ view: option, label, icon: Icon }) => (
        <button
          key={option}
          type="button"
          aria-pressed={view === option}
          onClick={() => {
            onChange(option);
          }}
          className={cn(
            "inline-flex items-center gap-1.5 px-2.5 text-xs font-medium transition-colors",
            view === option
              ? "bg-accent-soft text-accent"
              : "bg-surface-raised text-fg-muted hover:bg-surface-hover hover:text-fg",
          )}
        >
          <Icon aria-hidden className="size-4" />
          {label}
        </button>
      ))}
    </div>
  );
}
