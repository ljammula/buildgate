import { useUpdateRequestTicket } from "@/api/requestQueries";
import type { RequestSummary, RequestTicket } from "@/domain/request";
import { specAcceptanceCriteria } from "@/domain/specSkeleton";
import { getOperatorName } from "@/platform/operatorIdentity";

import { FileContentSection } from "./FileContentSection";
import type { EditorBinding } from "./useEditSession";
import { foldsContent, planSummary } from "./requestDetailLogic";

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
  return (
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
      specCriteria={specAcceptanceCriteria(request.spec)}
      {...(session === undefined ? {} : { session })}
      {...(foldsContent(request.state) ? { foldedSummary: planSummary(ticket.content) } : {})}
    />
  );
}
