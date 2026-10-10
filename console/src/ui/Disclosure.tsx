import { ChevronRight } from "lucide-react";
import { type ReactNode, useState } from "react";

import { cn } from "@/ui/cn";

export interface DisclosureProps {
  /** The name of the block: a heading inside the summary, so it stays findable by role and name. */
  readonly title: ReactNode;
  /** One line that says what is inside, so the operator rarely has to open it. */
  readonly summary?: ReactNode;
  /** Open until the operator closes it; a failure sets this, so it is first to be seen. */
  readonly defaultOpen?: boolean;
  /**
   * Controlled use: the disclosure is open exactly when this says so, and a
   * click on the summary only calls `onOpenChange` with the requested state.
   * Omit it and the disclosure keeps its own state.
   */
  readonly open?: boolean;
  /** Called with the new state when the operator toggles it, controlled or not. */
  readonly onOpenChange?: (open: boolean) => void;
  /** Draw the title and border in the failure colour. */
  readonly failed?: boolean;
  /** Without a card around it: for a disclosure inside another card. */
  readonly bare?: boolean;
  /** `null`: the title is plain text and the summary itself is named by it (role button), for a toggle that is not a section. */
  readonly headingLevel?: "h2" | "h3" | null;
  readonly children: ReactNode;
  readonly className?: string;
  readonly testId?: string;
}

/**
 * A native `<details>` with a one-line summary; its `<summary>` is the
 * keyboard-operable toggle and carries `aria-expanded`. The operator's own
 * toggle wins over `defaultOpen` from then on, so a live update that flips the
 * default (a gate that has just failed) never closes what they opened.
 */
export function Disclosure({
  title,
  summary,
  defaultOpen = false,
  open: controlledOpen,
  onOpenChange,
  failed = false,
  bare = false,
  headingLevel = "h2",
  children,
  className,
  testId,
}: DisclosureProps) {
  const [chosen, setChosen] = useState<boolean | null>(null);
  const controlled = controlledOpen !== undefined;
  const open = controlledOpen ?? chosen ?? defaultOpen;
  const Heading = headingLevel;
  return (
    <details
      open={open}
      data-open={open}
      data-failed={failed || undefined}
      {...(testId === undefined ? {} : { "data-testid": testId })}
      onToggle={(event) => {
        const now = event.currentTarget.open;
        if (now === open) return;
        if (!controlled) setChosen(now);
        onOpenChange?.(now);
      }}
      className={cn(
        "group min-w-0",
        !bare && "rounded-lg border bg-surface",
        !bare && (failed ? "border-tone-danger-border" : "border-border"),
        className,
      )}
    >
      <summary
        aria-expanded={open}
        {...(Heading === null ? { role: "button" } : {})}
        onClick={(event) => {
          // Controlled: the parent decides; the browser's own toggle is cancelled.
          if (!controlled) return;
          event.preventDefault();
          onOpenChange?.(!open);
        }}
        className={cn(
          "flex cursor-pointer list-none items-baseline gap-2 text-sm marker:hidden [&::-webkit-details-marker]:hidden",
          !bare && "px-4 py-3",
          bare && "py-1",
        )}
      >
        <ChevronRight
          aria-hidden="true"
          className="size-4 shrink-0 translate-y-0.5 text-fg-subtle transition-transform duration-100 motion-reduce:transition-none group-data-[open=true]:rotate-90"
        />
        {Heading === null ? (
          <span className={cn("shrink-0 font-medium", failed ? "text-tone-danger" : "text-fg")}>
            {title}
          </span>
        ) : (
          <Heading
            className={cn("shrink-0 font-semibold", failed ? "text-tone-danger" : "text-fg")}
          >
            {title}
          </Heading>
        )}
        {summary === undefined ? null : (
          <span className="min-w-0 truncate text-xs text-fg-muted">{summary}</span>
        )}
      </summary>
      <div className={cn("flex flex-col gap-3", !bare && "px-4 pb-4")}>{children}</div>
    </details>
  );
}
