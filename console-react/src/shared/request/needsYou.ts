import { requestStageGroupOf } from "@/domain/boardFilters";
import type { RequestSummary } from "@/domain/request";
import { sortedRequests } from "@/domain/requestOrder";

/**
 * Every request that waits on the operator, oldest wait first: spec and plan
 * review, oracle review, resume review, halted, quarantined, and a pr_review
 * whose pull requests all wait on their reviewer. The sidebar's Triage count
 * is its length and the Triage screen lists exactly these, so the two cannot
 * disagree.
 */
export function needsYouRequests(requests: readonly RequestSummary[]): RequestSummary[] {
  return sortedRequests(requests.filter((r) => requestStageGroupOf(r) === "review"));
}
