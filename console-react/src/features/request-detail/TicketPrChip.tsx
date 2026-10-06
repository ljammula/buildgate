import { statusForPRState } from "@/domain/status";
import { StatusChip } from "@/ui/StatusChip";

const PR_LABELS: Readonly<Record<string, string>> = {
  approved: "PR approved",
  ready: "PR ready for review",
  stacked: "PR stacked · merge its base first",
  changes_requested: "Changes requested",
  merged: "PR merged",
  closed: "PR closed",
  open: "PR open",
  draft: "PR draft",
};

/** One ticket's own PR review state, distinct from the request's stage chip. */
export function TicketPrChip({ prState }: { readonly prState: string }) {
  const label = Object.hasOwn(PR_LABELS, prState) ? (PR_LABELS[prState] ?? "") : `PR ${prState}`;
  return <StatusChip status={statusForPRState(prState)} label={label} />;
}
