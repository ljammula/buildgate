import { type ReactNode, createContext, useContext } from "react";

import { cn } from "@/ui/cn";

type LabelWidth = "sm" | "md" | "lg";

// Each width carries the density that goes with it, so the three places that
// draw a label/value list keep their look: a side-column digest (sm), a card's
// fields (md) and a wide settings-style list (lg, with a larger label).
const widths: Record<LabelWidth, { readonly grid: string; readonly label: string }> = {
  sm: { grid: "grid-cols-[5.5rem_minmax(0,1fr)] gap-x-2 gap-y-1.5", label: "text-xs leading-5" },
  md: { grid: "grid-cols-[11rem_minmax(0,1fr)] gap-x-4 gap-y-1.5", label: "text-xs leading-5" },
  lg: { grid: "grid-cols-[12rem_minmax(0,1fr)] gap-x-4 gap-y-2", label: "" },
};

const LabelClass = createContext<string>(widths.md.label);

export interface DescriptionListProps {
  /** The label column: sm 5.5rem, md 11rem (default), lg 12rem. */
  readonly labelWidth?: LabelWidth;
  /** `DescriptionItem`s. */
  readonly children: ReactNode;
  readonly className?: string;
}

/** A label/value list: one `dl`, so assistive technology reads each pair as one. */
export function DescriptionList({ labelWidth = "md", children, className }: DescriptionListProps) {
  const { grid, label } = widths[labelWidth];
  return (
    <LabelClass.Provider value={label}>
      <dl className={cn("grid text-sm", grid, className)}>{children}</dl>
    </LabelClass.Provider>
  );
}

export interface DescriptionItemProps {
  readonly label: ReactNode;
  readonly children: ReactNode;
  /** Monospace, for ids, SHAs and paths: small, and breaks anywhere. */
  readonly mono?: boolean;
}

/** One `<dt>`/`<dd>` pair, a direct child of the list's grid. */
export function DescriptionItem({ label, children, mono = false }: DescriptionItemProps) {
  const labelClass = useContext(LabelClass);
  return (
    <>
      <dt className={cn("text-fg-muted", labelClass)}>{label}</dt>
      <dd className={cn("min-w-0 break-words text-fg", mono && "font-mono text-xs break-all")}>
        {children}
      </dd>
    </>
  );
}
