import { Button } from "@/ui/Button";
import { cn } from "@/ui/cn";

export interface OlderHiddenNoteProps {
  /** Finished requests the time window left out; nothing is drawn for 0. */
  readonly count: number;
  /** Widens the window to all time. */
  readonly onShowAllTime: () => void;
  readonly className?: string;
}

/**
 * "12 older hidden" beside what the window cut short, with the way out: the
 * window never hides finished work without saying so.
 */
export function OlderHiddenNote({ count, onShowAllTime, className }: OlderHiddenNoteProps) {
  if (count === 0) return null;
  return (
    <p
      data-testid="older-hidden"
      className={cn("text-fg-muted flex flex-wrap items-center gap-x-2 text-xs", className)}
    >
      <span>{`${count} older hidden`}</span>
      <Button size="sm" variant="link" onClick={onShowAllTime}>
        All time
      </Button>
    </p>
  );
}
