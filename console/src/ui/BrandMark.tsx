import { cn } from "@/ui/cn";

/** Where the build serves the full mark: public/buildgate.svg, at the bundle's root. */
export const brandMarkUrl = "/buildgate.svg";

export interface BrandMarkProps {
  readonly className?: string;
}

/**
 * The small cut of the Buildgate mark, for sizes where the joints between the
 * full drawing's bricks vanish: two lintel bricks, two pillars and the check
 * in the opening, on no tile, with the check in the text colour so it follows
 * the theme. The full drawing (public/buildgate.svg) stays the tab and
 * notification icon. Decorative: the product name is always written beside it.
 */
export function BrandMark({ className }: BrandMarkProps) {
  return (
    <svg viewBox="0 0 24 24" aria-hidden="true" className={cn("shrink-0 text-fg", className)}>
      <g className="fill-brand">
        <rect x="3" y="3" width="8.5" height="5" rx="1.25" />
        <rect x="12.5" y="3" width="8.5" height="5" rx="1.25" />
        <rect x="3" y="9.5" width="5" height="11.5" rx="1.25" />
        <rect x="16" y="9.5" width="5" height="11.5" rx="1.25" />
      </g>
      <path
        d="M9.6 15.2l1.9 1.9 3.2-4.2"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.8"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
    </svg>
  );
}
