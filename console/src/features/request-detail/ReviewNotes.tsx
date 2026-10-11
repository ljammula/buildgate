import type { RequestSummary } from "@/domain/request";
import { Callout } from "@/ui/Feedback";
import { TextWithCode } from "@/ui/TextWithCode";

/** Whether the request waits at a gate the factory left notes on. */
export function hasReviewNotes(request: RequestSummary): boolean {
  return (
    (request.state === "spec_review" || request.state === "plan_review") &&
    request.reviewNotes.length > 0
  );
}

/**
 * The factory's notes on the spec or the plan under review. They come from
 * its reading of the text and can be wrong, so they decide nothing: the
 * operator approves, or requests changes with a note as the feedback.
 */
export function ReviewNotes({ request }: { readonly request: RequestSummary }) {
  if (!hasReviewNotes(request)) return null;
  return (
    <Callout tone="neutral" title="Notes from the factory" data-testid="review-notes">
      <ul className="list-disc pl-5">
        {request.reviewNotes.map((note) => (
          <li key={note}>
            <TextWithCode text={note} copy={false} />
          </li>
        ))}
      </ul>
      <p className="text-fg-muted text-sm">
        Advice only, and it can be wrong. Approve as it is, or request changes and say what to fix.
      </p>
    </Callout>
  );
}
