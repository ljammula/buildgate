import * as TooltipPrimitive from "@radix-ui/react-tooltip";
import type { ReactNode } from "react";

import { cn } from "@/ui/cn";

export const TooltipProvider = TooltipPrimitive.Provider;

export interface TooltipProps {
  readonly content: ReactNode;
  /** The element that triggers the tooltip; it must accept a ref and focus. */
  readonly children: ReactNode;
  readonly side?: "top" | "right" | "bottom" | "left";
  readonly className?: string;
}

export function Tooltip({ content, children, side = "top", className }: TooltipProps) {
  return (
    <TooltipPrimitive.Root>
      <TooltipPrimitive.Trigger asChild>{children}</TooltipPrimitive.Trigger>
      <TooltipPrimitive.Portal>
        <TooltipPrimitive.Content
          side={side}
          sideOffset={6}
          className={cn(
            "z-50 max-w-xs rounded-md border border-border bg-surface-raised px-2 py-1 text-xs text-fg shadow-popover",
            className,
          )}
        >
          {content}
        </TooltipPrimitive.Content>
      </TooltipPrimitive.Portal>
    </TooltipPrimitive.Root>
  );
}
