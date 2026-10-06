import { useApi } from "@/api/ApiProvider";
import type { RequestSummary } from "@/domain/request";
import { OracleReviewPanel } from "@/shared/oracle/OracleReviewPanel";
import { TicketOraclePanel } from "@/shared/oracle/TicketOraclePanel";
import { Callout } from "@/ui/Feedback";

import { Panel } from "./Panel";
import { type RequestDialogs, awaitsTicketOracle } from "./useRequestDialogs";

export interface ReviewSectionProps {
  readonly request: RequestSummary;
  readonly dialogs: RequestDialogs;
  readonly detailLoaded: boolean;
  readonly anyEditorOpen: boolean;
}

/**
 * The review notes and the oracle files an approval pins: why Approve is
 * unavailable, the oracle_review panel (which owns its Approve), and the
 * plan_review ticket-oracle panel (which Approve waits for).
 */
export function ReviewSection({
  request,
  dialogs,
  detailLoaded,
  anyEditorOpen,
}: ReviewSectionProps) {
  const { canWrite } = useApi();
  const hasContent =
    !canWrite ||
    !detailLoaded ||
    request.state === "oracle_review" ||
    awaitsTicketOracle(request) ||
    anyEditorOpen;
  // A card with nothing in it (a spec review the operator can already act on) is noise.
  if (!hasContent) return null;
  return (
    <Panel title="Review">
      {!canWrite ? (
        <Callout tone="neutral" title="Note">
          This console cannot write here: the server has writes disabled for this origin and no
          override token is configured for this console build. Ask the operator running{" "}
          <code className="font-mono">factoryd serve</code> to bind it to loopback (or configure an
          override token) to enable approve/reject from here.
        </Callout>
      ) : null}
      {canWrite && !detailLoaded ? (
        <p className="text-fg-muted text-sm">
          Loading the full spec/plan content before enabling approve/reject...
        </p>
      ) : null}
      {request.state === "oracle_review" ? (
        <OracleReviewPanel
          key={`oracle-review-${request.id}`}
          request={request}
          canAct={!dialogs.acting && detailLoaded}
          onApprove={dialogs.approveOracle}
        />
      ) : null}
      {awaitsTicketOracle(request) ? (
        <TicketOraclePanel
          key={`ticket-oracle-${request.id}-${request.updatedAt}-${dialogs.ticketOracleNonce}`}
          request={request}
          onChanged={dialogs.onTicketOracleChanged}
        />
      ) : null}
      {anyEditorOpen ? (
        <p className="text-fg-muted text-sm">
          Approve/Request changes are disabled while an edit is open -- Save or Cancel it first.
        </p>
      ) : null}
    </Panel>
  );
}
