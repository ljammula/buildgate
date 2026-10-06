import { type VariantProps, cva } from "class-variance-authority";
import type { HTMLAttributes, Ref } from "react";

import type { StatusTone } from "@/domain/status";
import { cn } from "@/ui/cn";
import { toneClasses } from "@/ui/tone";

const badgeVariants = cva(
  "inline-flex items-center gap-1 rounded-full border px-2 py-0.5 text-xs font-medium whitespace-nowrap [&_svg]:size-3",
  {
    variants: { variant: { soft: "", outline: "bg-transparent" } },
    defaultVariants: { variant: "soft" },
  },
);

export interface BadgeProps
  extends HTMLAttributes<HTMLSpanElement>, VariantProps<typeof badgeVariants> {
  readonly tone?: StatusTone;
  readonly ref?: Ref<HTMLSpanElement>;
}

export function Badge({ tone = "neutral", variant, className, ...props }: BadgeProps) {
  const classes = toneClasses[tone];
  return (
    <span
      className={cn(
        badgeVariants({ variant }),
        classes.text,
        classes.border,
        variant !== "outline" && classes.soft,
        className,
      )}
      {...props}
    />
  );
}
