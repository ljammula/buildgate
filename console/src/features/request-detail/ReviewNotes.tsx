import type { RequestSummary } from "@/domain/request";
import { planReviewSkipped } from "./requestDetailLogic";
import { Callout } from "@/ui/Feedback";
import { TextWithCode } from "@/ui/TextWithCode";

/**
 * Whether the request has notes from the factory to show: at a spec or plan
 * review, or while building on a plan the factory approved itself.
 */
export function hasReviewNotes(request: RequestSummary): boolean {
  const shown =
    request.state === "spec_review" ||
    request.state === "plan_review" ||
    (request.state === "building" && planReviewSkipped(request));
  return shown && request.reviewNotes.length > 0;
}

/**
 * The factory's notes on the spec or the plan under review. They come from
 * its reading of the text and can be wrong, so they decide nothing: the
 * operator approves, or requests changes with a note as the feedback.
 */
export function ReviewNotes({ request }: { readonly request: RequestSummary }) {
  if (!hasReviewNotes(request)) return null;
  const skipped = request.state === "building";
  return (
    <Callout
      tone="neutral"
      title={skipped ? "Plan notes (plan review skipped)" : "Notes from the factory"}
      data-testid="review-notes"
    >
      <ul className="list-disc pl-5">
        {request.reviewNotes.map((note) => (
          <li key={note}>
            <TextWithCode text={note} copy={false} />
          </li>
        ))}
      </ul>
      <p className="text-fg-muted text-sm">
        {skipped
          ? "The factory approved this one-ticket plan itself; no person reviewed it. Advice only, and it can be wrong."
          : "Advice only, and it can be wrong. Approve as it is, or request changes and say what to fix."}
      </p>
    </Callout>
  );
}
