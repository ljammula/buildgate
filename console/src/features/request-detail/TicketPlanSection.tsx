import { useUpdateRequestTicket } from "@/api/requestQueries";
import type { RequestSummary, RequestTicket } from "@/domain/request";
import { getOperatorName } from "@/platform/operatorIdentity";

import { ticketRelPath } from "@/domain/contentHash";
import { usePendingComments } from "@/shared/request/usePendingComments";
import { FileContentSection } from "./FileContentSection";
import type { EditorBinding } from "./useEditSession";
import { foldsContent, planSummary, sizeLine } from "./requestDetailLogic";

export interface TicketPlanSectionProps {
  readonly request: RequestSummary;
  readonly ticket: RequestTicket;
  readonly editable: boolean;
  readonly editing: boolean;
  readonly onStartEdit: () => void;
  readonly onStopEdit: () => void;
  /** The server's current content of THIS ticket, after a refetch. */
  readonly onFetchCurrent: () => Promise<string>;
  readonly session?: EditorBinding;
}

/** One ticket's plan file, editable and whole in plan_review; one closed line once the approval is behind it. */
export function TicketPlanSection({
  request,
  ticket,
  editable,
  editing,
  onStartEdit,
  onStopEdit,
  onFetchCurrent,
  session,
}: TicketPlanSectionProps) {
  const update = useUpdateRequestTicket(request.id, ticket.index);
  const pending = usePendingComments(request);
  const commentPath = ticketRelPath(ticket.specPath);
  const size = sizeLine(ticket, request.planBudgetLines);
  return (
    <>
      {size !== "" ? (
        <p className="text-fg-muted px-1 text-sm" data-testid={`ticket-size-${ticket.index}`}>
          Ticket {ticket.index}: {size}
        </p>
      ) : null}
      <FileContentSection
        title={`Ticket ${ticket.index} plan`}
        path={ticket.specPath}
        fullPath={ticket.fullPath}
        content={ticket.content}
        editable={editable}
        editing={editing}
        onStartEdit={onStartEdit}
        onStopEdit={onStopEdit}
        onSave={async (content, baseSha256) => {
          await update.mutateAsync({ content, baseSha256, by: getOperatorName() });
        }}
        onFetchCurrent={onFetchCurrent}
        structure="ticket"
        {...(editable && commentPath !== null ? { commentPath, onComment: pending.add } : {})}
        specCriteria={request.criteria}
        {...(session === undefined ? {} : { session })}
        {...(foldsContent(request.state) ? { foldedSummary: planSummary(ticket.content) } : {})}
      />
    </>
  );
}
