import { Loader2 } from "lucide-react";
import type { HTMLAttributes, ReactNode } from "react";

import type { StatusTone } from "@/domain/status";
import { cn } from "@/ui/cn";
import { toneClasses } from "@/ui/tone";

export interface SpinnerProps extends HTMLAttributes<HTMLSpanElement> {
  /** Accessible name; default "Loading". */
  readonly label?: string;
}

export function Spinner({ label = "Loading", className, ...props }: SpinnerProps) {
  return (
    <span role="status" aria-label={label} className={cn("inline-flex", className)} {...props}>
      <Loader2 className="size-4 animate-spin text-fg-muted" aria-hidden="true" />
    </span>
  );
}

export function Skeleton({ className, ...props }: HTMLAttributes<HTMLDivElement>) {
  return (
    <div
      aria-hidden="true"
      className={cn("h-4 animate-pulse rounded-md bg-surface-hover", className)}
      {...props}
    />
  );
}

export interface EmptyStateProps {
  readonly title: string;
  readonly children?: ReactNode;
  readonly action?: ReactNode;
  readonly className?: string;
}

export function EmptyState({ title, children, action, className }: EmptyStateProps) {
  return (
    <div
      className={cn(
        "flex flex-col items-center gap-2 rounded-lg border border-dashed border-border px-6 py-10 text-center",
        className,
      )}
    >
      <p className="text-sm font-medium text-fg">{title}</p>
      {children ? <div className="max-w-md text-sm text-fg-muted">{children}</div> : null}
      {action ? <div className="mt-2">{action}</div> : null}
    </div>
  );
}

export interface CalloutProps extends Omit<HTMLAttributes<HTMLDivElement>, "title"> {
  readonly tone: StatusTone;
  readonly title?: ReactNode;
}

export function Callout({ tone, title, children, className, ...props }: CalloutProps) {
  const classes = toneClasses[tone];
  return (
    <div
      role={tone === "danger" ? "alert" : "status"}
      className={cn("rounded-md border px-3 py-2 text-sm", classes.soft, classes.border, className)}
      {...props}
    >
      {title ? <p className={cn("font-medium", classes.text)}>{title}</p> : null}
      <div className="text-fg">{children}</div>
    </div>
  );
}
