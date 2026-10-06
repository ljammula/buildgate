import type { HTMLAttributes } from "react";

import { cn } from "@/ui/cn";

export function Kbd({ className, ...props }: HTMLAttributes<HTMLElement>) {
  return (
    <kbd
      className={cn(
        "inline-flex h-5 min-w-5 items-center justify-center rounded-sm border border-border bg-surface-sunken px-1 text-xs text-fg-muted",
        className,
      )}
      {...props}
    />
  );
}
