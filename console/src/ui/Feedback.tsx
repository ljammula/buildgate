import {
  CircleAlert,
  CircleCheck,
  Info,
  Inbox,
  Loader2,
  TriangleAlert,
  type LucideIcon,
} from "lucide-react";
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
  readonly icon?: LucideIcon;
  readonly className?: string;
}

export function EmptyState({
  title,
  children,
  action,
  icon: Icon = Inbox,
  className,
}: EmptyStateProps) {
  return (
    <div
      className={cn(
        "flex flex-col items-center gap-1.5 rounded-lg border border-dashed border-border px-6 py-10 text-center",
        className,
      )}
    >
      <Icon aria-hidden="true" className="mb-1 size-6 text-fg-subtle" />
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

const calloutIcons: Readonly<Record<StatusTone, LucideIcon>> = {
  warning: TriangleAlert,
  danger: CircleAlert,
  success: CircleCheck,
  info: Info,
  neutral: Info,
  neutralOnDark: Info,
  muted: Info,
};

export function Callout({ tone, title, children, className, ...props }: CalloutProps) {
  const classes = toneClasses[tone];
  const Icon = calloutIcons[tone];
  return (
    <div
      role={tone === "danger" ? "alert" : "status"}
      className={cn(
        "flex gap-2.5 rounded-md border px-3 py-2 text-sm",
        title ? "items-start" : "items-center",
        // Quiet: no tinted fill. A warning or a failure keeps its tone on the
        // border, so it is not told from a note by a 14px icon alone.
        "bg-surface",
        tone === "warning" || tone === "danger" ? classes.border : "border-border",
        className,
      )}
      {...props}
    >
      <Icon aria-hidden="true" className={cn("size-4 shrink-0", title && "mt-0.5", classes.text)} />
      <div className="min-w-0 flex-1">
        {title ? <p className={cn("font-medium", classes.text)}>{title}</p> : null}
        <div className="text-fg">{children}</div>
      </div>
    </div>
  );
}
