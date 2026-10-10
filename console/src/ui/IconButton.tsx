import { Button, type ButtonProps } from "@/ui/Button";
import { Tooltip } from "@/ui/Tooltip";

export interface IconButtonProps extends Omit<ButtonProps, "aria-label" | "size" | "title"> {
  /** The accessible name and the tooltip: an icon alone does not say what the button does. */
  readonly label: string;
}

/**
 * A button whose only content is an icon: named by `label` (its `aria-label`,
 * so the tooltip is never the only name) and showing the same text on hover
 * and focus. With `asChild` the child (a router Link) keeps being a link.
 */
export function IconButton({ label, variant = "ghost", ...props }: IconButtonProps) {
  return (
    <Tooltip content={label}>
      <Button variant={variant} size="icon" aria-label={label} {...props} />
    </Tooltip>
  );
}
