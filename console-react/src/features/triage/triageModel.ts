import type { RequestSummary } from "@/domain/request";
import { sortedRequests } from "@/domain/requestOrder";

export type NonEmptyRequests = readonly [RequestSummary, ...RequestSummary[]];

export function isNonEmpty(requests: readonly RequestSummary[]): requests is NonEmptyRequests {
  return requests.length > 0;
}

/**
 * The requests triage lists: spec_review and plan_review, oldest wait first.
 * Literal states, not the board's "needs you" grouping: that also covers
 * `halted`, but this screen exists for the approve/reject decision, which the
 * server only accepts from these two states, so listing a halted request
 * would offer an action that just fails. oracle_review is excluded on
 * purpose too: approving it needs the request page's oracle panel, which
 * shows every oracle file and sends the hashes it displayed, so a triage
 * keystroke would approve blind.
 */
export function triageRequests(requests: readonly RequestSummary[]): RequestSummary[] {
  return sortedRequests(
    requests.filter((r) => r.state === "spec_review" || r.state === "plan_review"),
  );
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
