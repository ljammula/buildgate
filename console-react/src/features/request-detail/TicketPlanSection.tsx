import { useUpdateRequestTicket } from "@/api/requestQueries";
import type { RequestSummary, RequestTicket } from "@/domain/request";

import { FileContentSection } from "./FileContentSection";

export interface TicketPlanSectionProps {
  readonly request: RequestSummary;
  readonly ticket: RequestTicket;
  readonly editable: boolean;
  readonly editing: boolean;
  readonly onStartEdit: () => void;
  readonly onStopEdit: () => void;
  /** The server's current content of THIS ticket, after a refetch. */
  readonly onFetchCurrent: () => Promise<string>;
}

/** One ticket's plan file, editable in plan_review. */
export function TicketPlanSection({
  request,
  ticket,
  editable,
  editing,
  onStartEdit,
  onStopEdit,
  onFetchCurrent,
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
        await update.mutateAsync({ content, baseSha256 });
      }}
      onFetchCurrent={onFetchCurrent}
    />
  );
}
