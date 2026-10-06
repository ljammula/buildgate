import { cn } from "@/ui/cn";

/** Where the build serves the mark: public/buildgate.svg, at the bundle's root. */
export const brandMarkUrl = "/buildgate.svg";

export interface BrandMarkProps {
  readonly className?: string;
}

/**
 * The Buildgate mark: the one drawing in assets/brand/buildgate.svg, which
 * the tab icon and desktop notifications also carry. It is decorative here:
 * the product name is always written beside it.
 */
export function BrandMark({ className }: BrandMarkProps) {
  return <img src={brandMarkUrl} alt="" aria-hidden className={cn("shrink-0", className)} />;
}
