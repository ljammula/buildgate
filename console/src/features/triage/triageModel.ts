import type { RequestSummary } from "@/domain/request";
import { needsYouRequests } from "@/shared/request/needsYou";

export type NonEmptyRequests = readonly [RequestSummary, ...RequestSummary[]];

export function isNonEmpty(requests: readonly RequestSummary[]): requests is NonEmptyRequests {
  return requests.length > 0;
}

/**
 * The requests triage lists: every request that needs the operator (the same
 * set as the sidebar's count, `needsYouRequests`), oldest wait first.
 */
export function triageRequests(requests: readonly RequestSummary[]): RequestSummary[] {
  return needsYouRequests(requests);
}

/**
 * Whether Approve and Reject act on the request here: only spec_review and
 * plan_review, whose spec or plan is shown in full. oracle_review is decided
 * on the request page, where the oracle panel shows every file and sends the
 * hashes it displayed, so a triage keystroke would approve blind; the other
 * states have their own recovery actions there.
 */
export function decidesInPlace(request: RequestSummary): boolean {
  return request.state === "spec_review" || request.state === "plan_review";
}

const reasonFallback: Readonly<Record<string, string>> = {
  oracle_review: "The drafted acceptance tests wait for your review.",
  resume_review: "A resume plan waits for your approval.",
  halted: "The build halted and needs your decision.",
  quarantined: "A ticket was quarantined and needs your triage.",
  pr_review: "Its pull requests wait on their reviewer.",
};

/**
 * The one sentence on why a request needs the operator: the server's next
 * step, else a fixed line per state. A review state's next step names CLI
 * commands for buttons the request page has, so those take the fixed line.
 */
export function triageReason(request: RequestSummary): string {
  const reviewState = request.state === "oracle_review" || request.state === "resume_review";
  if (request.nextAction !== "" && !reviewState) return request.nextAction;
  return reasonFallback[request.state] ?? "This request waits on you.";
}

/**
 * The review artifact: spec.md for spec_review, the ticket plans for
 * plan_review. Chosen by state, not by which field is non-empty: spec.md is
 * already approved and non-empty by plan_review, so choosing on content
 * always showed the already-decided spec instead of the plans under review.
 */
export function artifactContent(detail: RequestSummary): string {
  if (detail.state !== "plan_review") return detail.spec;
  return detail.tickets
    .filter((ticket) => ticket.content !== "")
    .map((ticket) => `--- ticket ${ticket.index} ---\n${ticket.content}\n\n`)
    .join("");
}

/** Whether a key press belongs to a text field and must not trigger a shortcut. */
export function isTypingTarget(target: EventTarget | null): boolean {
  if (!(target instanceof HTMLElement)) return false;
  return (
    target.isContentEditable ||
    target instanceof HTMLInputElement ||
    target instanceof HTMLTextAreaElement ||
    target instanceof HTMLSelectElement
  );
}
