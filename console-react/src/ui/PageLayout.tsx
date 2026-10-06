import type { HTMLAttributes, ReactNode } from "react";

import { cn } from "@/ui/cn";

export interface PageHeaderProps {
  readonly title: string;
  readonly description?: ReactNode;
  readonly actions?: ReactNode;
  readonly breadcrumbs?: ReactNode;
  readonly className?: string;
}

/** Renders the page's single h1. */
export function PageHeader({ title, description, actions, breadcrumbs, className }: PageHeaderProps) {
  return (
    <header className={cn("flex flex-col gap-2 border-b border-border px-6 py-5", className)}>
      {breadcrumbs ? <div className="text-xs text-fg-muted">{breadcrumbs}</div> : null}
      <div className="flex items-start justify-between gap-4">
        <div className="min-w-0">
          <h1 className="text-lg font-semibold text-fg">{title}</h1>
          {description ? <p className="mt-1 text-sm text-fg-muted">{description}</p> : null}
        </div>
        {actions ? <div className="flex shrink-0 items-center gap-2">{actions}</div> : null}
      </div>
    </header>
  );
}

export function PageBody({ className, ...props }: HTMLAttributes<HTMLDivElement>) {
  return <div className={cn("flex flex-col gap-6 px-6 py-5", className)} {...props} />;
}

export interface SectionProps {
  readonly title: string;
  readonly actions?: ReactNode;
  readonly children: ReactNode;
  readonly className?: string;
}

export function Section({ title, actions, children, className }: SectionProps) {
  return (
    <section className={cn("flex flex-col gap-3", className)}>
      <div className="flex items-center justify-between gap-3">
        <h2 className="text-base font-semibold text-fg">{title}</h2>
        {actions ? <div className="flex items-center gap-2">{actions}</div> : null}
      </div>
      {children}
    </section>
  );
}
