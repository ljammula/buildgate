import { Check, Copy } from "lucide-react";

import { IconButton } from "@/ui/IconButton";
import { cn } from "@/ui/cn";
import { useCopied } from "@/ui/useCopied";

export interface CopyButtonProps {
  /** What is written to the clipboard. */
  readonly text: string;
  /** The accessible name while idle, e.g. "Copy request id"; it reads "Copied" for a moment after a copy. */
  readonly label: string;
  /** "sm": a 20px inline glyph beside text (with a tooltip); "md": a ghost icon button in a card. */
  readonly size?: "sm" | "md";
  readonly writeText?: ((text: string) => Promise<void>) | undefined;
  readonly className?: string;
}

/** An icon-only copy button: the Copy glyph, then a check and the name "Copied". */
export function CopyButton({ text, label, size = "md", writeText, className }: CopyButtonProps) {
  const { copied, copy } = useCopied(writeText);
  const name = copied ? "Copied" : label;
  const glyph = copied ? (
    <Check aria-hidden="true" className={size === "sm" ? "size-3" : undefined} />
  ) : (
    <Copy aria-hidden="true" className={size === "sm" ? "size-3" : undefined} />
  );
  if (size === "sm") {
    return (
      <button
        type="button"
        aria-label={name}
        title={name}
        onClick={() => void copy(text)}
        className={cn(
          "inline-grid size-5 shrink-0 place-items-center rounded text-fg-subtle hover:bg-surface-hover hover:text-fg focus-visible:outline-ring",
          className,
        )}
      >
        {glyph}
      </button>
    );
  }
  return (
    <IconButton label={name} onClick={() => void copy(text)} className={className}>
      {glyph}
    </IconButton>
  );
}
