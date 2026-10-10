import type { HTMLAttributes, Ref } from "react";

import { cn } from "@/ui/cn";

interface DivProps extends HTMLAttributes<HTMLDivElement> {
  readonly ref?: Ref<HTMLDivElement>;
}

export type CardProps = DivProps;

export function Card({ className, ...props }: CardProps) {
  return (
    <div
      className={cn("rounded-lg border border-border bg-surface shadow-card", className)}
      {...props}
    />
  );
}

export type CardHeaderProps = DivProps;

export function CardHeader({ className, ...props }: CardHeaderProps) {
  return (
    <div
      className={cn(
        "flex items-center justify-between gap-3 border-b border-border px-4 py-3",
        className,
      )}
      {...props}
    />
  );
}

export interface CardTitleProps extends HTMLAttributes<HTMLHeadingElement> {
  readonly as?: "h2" | "h3" | "h4";
  readonly ref?: Ref<HTMLHeadingElement>;
}

export function CardTitle({ as: Heading = "h2", className, ...props }: CardTitleProps) {
  return <Heading className={cn("text-sm font-semibold text-fg", className)} {...props} />;
}

export type CardBodyProps = DivProps;

export function CardBody({ className, ...props }: CardBodyProps) {
  return <div className={cn("p-4", className)} {...props} />;
}

export type CardFooterProps = DivProps;

export function CardFooter({ className, ...props }: CardFooterProps) {
  return (
    <div
      className={cn(
        "flex items-center justify-end gap-2 border-t border-border px-4 py-3",
        className,
      )}
      {...props}
    />
  );
}
