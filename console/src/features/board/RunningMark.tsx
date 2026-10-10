import { Loader2 } from "lucide-react";

/**
 * The mark of work the worker is doing now: a small spinner, and the word for
 * assistive technology. It only spins; under reduced motion it is drawn
 * still, and the word still says it.
 */
export function RunningMark() {
  return (
    <>
      <Loader2
        aria-hidden
        className="text-tone-info size-3.5 shrink-0 animate-spin motion-reduce:animate-none"
      />
      <span className="sr-only">running</span>
    </>
  );
}
