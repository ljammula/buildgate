import type { ReactNode } from "react";

import { cn } from "@/ui/cn";

/** A label/value list: one `dl`, so assistive technology reads each pair as one. */
export function Fields({ children, className }: { children: ReactNode; className?: string }) {
  return (
    <dl className={cn("grid grid-cols-[11rem_minmax(0,1fr)] gap-x-4 gap-y-1.5 text-sm", className)}>
      {children}
    </dl>
  );
}

export interface FieldProps {
  readonly label: string;
  readonly children: ReactNode;
  /** Monospace, for ids, SHAs and paths. */
  readonly mono?: boolean;
}

export function Field({ label, children, mono = false }: FieldProps) {
  return (
    <>
      <dt className="text-fg-muted">{label}</dt>
      <dd className={cn("min-w-0 break-words text-fg", mono && "font-mono text-xs")}>{children}</dd>
    </>
  );
}
