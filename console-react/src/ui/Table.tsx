import type {
  HTMLAttributes,
  Ref,
  TableHTMLAttributes,
  TdHTMLAttributes,
  ThHTMLAttributes,
} from "react";

import { cn } from "@/ui/cn";

export interface TableProps extends TableHTMLAttributes<HTMLTableElement> {
  readonly ref?: Ref<HTMLTableElement>;
}

/** The rounded border around a table, so the rows' dividers stop short of the page. */
export function TableFrame({ className, ...props }: HTMLAttributes<HTMLDivElement>) {
  return (
    <div
      className={cn("overflow-x-auto rounded-lg border border-border bg-surface", className)}
      {...props}
    />
  );
}

export function Table({ className, ...props }: TableProps) {
  return <table className={cn("w-full border-collapse text-sm", className)} {...props} />;
}

export function TableHead({ className, ...props }: HTMLAttributes<HTMLTableSectionElement>) {
  return <thead className={cn("sticky top-0 z-10 bg-surface-sunken", className)} {...props} />;
}

export function TableBody({ className, ...props }: HTMLAttributes<HTMLTableSectionElement>) {
  return <tbody className={className} {...props} />;
}

export interface TableRowProps extends HTMLAttributes<HTMLTableRowElement> {
  readonly selected?: boolean;
}

export function TableRow({ selected = false, className, ...props }: TableRowProps) {
  return (
    <tr
      aria-selected={selected || undefined}
      className={cn(
        "h-9 border-b border-border transition-colors last:border-b-0 hover:bg-surface-hover",
        selected && "bg-accent-soft",
        className,
      )}
      {...props}
    />
  );
}

export function TableHeaderCell({ className, ...props }: ThHTMLAttributes<HTMLTableCellElement>) {
  return (
    <th
      scope="col"
      className={cn(
        "h-8 border-b border-border px-3 text-left text-xs font-medium whitespace-nowrap text-fg-muted",
        className,
      )}
      {...props}
    />
  );
}

export function TableCell({ className, ...props }: TdHTMLAttributes<HTMLTableCellElement>) {
  return <td className={cn("px-3 py-2 align-middle text-fg", className)} {...props} />;
}
