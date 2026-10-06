import * as MenuPrimitive from "@radix-ui/react-dropdown-menu";
import type { ComponentProps } from "react";

import { cn } from "@/ui/cn";

export const DropdownMenu = MenuPrimitive.Root;
export const DropdownMenuTrigger = MenuPrimitive.Trigger;

export function DropdownMenuContent({
  className,
  sideOffset = 4,
  ...props
}: ComponentProps<typeof MenuPrimitive.Content>) {
  return (
    <MenuPrimitive.Portal>
      <MenuPrimitive.Content
        sideOffset={sideOffset}
        className={cn(
          "z-50 min-w-40 rounded-md border border-border bg-surface-raised p-1 text-sm text-fg shadow-popover",
          className,
        )}
        {...props}
      />
    </MenuPrimitive.Portal>
  );
}

export function DropdownMenuItem({
  className,
  ...props
}: ComponentProps<typeof MenuPrimitive.Item>) {
  return (
    <MenuPrimitive.Item
      className={cn(
        "flex h-7 cursor-default items-center gap-2 rounded-sm px-2 outline-none select-none data-[disabled]:opacity-50 data-[highlighted]:bg-surface-hover [&_svg]:size-4",
        className,
      )}
      {...props}
    />
  );
}

export function DropdownMenuSeparator({
  className,
  ...props
}: ComponentProps<typeof MenuPrimitive.Separator>) {
  return <MenuPrimitive.Separator className={cn("my-1 h-px bg-border", className)} {...props} />;
}

export function DropdownMenuLabel({
  className,
  ...props
}: ComponentProps<typeof MenuPrimitive.Label>) {
  return (
    <MenuPrimitive.Label className={cn("px-2 py-1 text-xs text-fg-subtle", className)} {...props} />
  );
}
