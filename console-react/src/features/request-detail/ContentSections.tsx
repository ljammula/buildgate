import type { RequestSummary } from "@/domain/request";

import { SpecSection } from "./SpecSection";
import { TicketPlanSection } from "./TicketPlanSection";
import type { ContentFile } from "./requestDetailLogic";
import type { EditorBinding } from "./useEditSession";

export interface ContentSectionsProps {
  readonly request: RequestSummary;
  readonly files: readonly ContentFile[];
  /** The id of the file being edited, or null. */
  readonly editing: string | null;
  readonly setEditing: (id: string | null) => void;
  /** The open editor's notice, dirty reporting and save lock. */
  readonly session: EditorBinding;
  /** Refetches the request and returns the fresh record (409 conflict resolution). */
  readonly refetchRequest: () => Promise<RequestSummary>;
}

/** The spec or the tickets' plan files, each with its own editor. */
export function ContentSections({
  request,
  files,
  editing,
  setEditing,
  session,
  refetchRequest,
}: ContentSectionsProps) {
  return (
    <>
      {files.map((file) => {
        const shared = {
          request,
          editable: file.editable,
          editing: editing === file.id,
          ...(editing === file.id ? { session } : {}),
          onStartEdit: () => {
            setEditing(file.id);
          },
          onStopEdit: () => {
            setEditing(null);
          },
        };
        const ticket = file.ticket;
        if (ticket === null) {
          return (
            <SpecSection
              key={file.id}
              {...shared}
              onFetchCurrent={async () => (await refetchRequest()).spec}
            />
          );
        }
        return (
          <TicketPlanSection
            key={file.id}
            {...shared}
            ticket={ticket}
            // The ticket is gone from the fresh fetch (a renumbered plan):
            // nothing is left to diff against, and the conflict notice falls
            // back to the hash alone.
            onFetchCurrent={async () =>
              (await refetchRequest()).tickets.find((t) => t.index === ticket.index)?.content ?? ""
            }
          />
        );
      })}
    </>
  );
}
