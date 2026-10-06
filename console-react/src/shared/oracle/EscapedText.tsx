import { segmentEscapes } from "@/domain/textEscape";
import { cn } from "@/ui/cn";

export interface EscapedTextProps {
  /** Untrusted text: hidden characters and invalid bytes are written out. */
  readonly text: string;
  readonly className?: string;
}

/**
 * Text with hidden characters, bidi controls and invalid bytes made visible
 * (`\u{202E}`, `\xFF`), the synthetic escapes highlighted so they cannot be
 * mistaken for characters of the source. Newlines and spacing are kept.
 */
export function EscapedText({ text, className }: EscapedTextProps) {
  const segments = segmentEscapes(text);
  return (
    <span className={cn("block break-words whitespace-pre-wrap", className)}>
      {segments.map((segment, index) =>
        segment.escaped ? (
          <span
            // Segments are positional and never reordered.
            key={index}
            className="bg-tone-danger-soft text-tone-danger rounded-sm font-bold"
          >
            {segment.text}
          </span>
        ) : (
          segment.text
        ),
      )}
    </span>
  );
}
