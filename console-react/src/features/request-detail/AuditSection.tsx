import type { RequestSummary } from "@/domain/request";

import { Panel } from "./Panel";
import { RejectionHistory } from "./RejectionHistory";
import { approvedLine } from "./requestDetailLogic";

/** Who approved the request and when, and its rejection history. Hidden when there is neither. */
export function AuditSection({ request }: { readonly request: RequestSummary }) {
  if (request.approvedBy === "" && request.rejections.length === 0) return null;
  return (
    <Panel title="Audit">
      {request.approvedBy === "" ? null : (
        <p className="text-sm">{approvedLine(request.approvedBy, request.approvedAt)}</p>
      )}
      {request.rejections.length === 0 ? null : (
        <RejectionHistory rejections={request.rejections} />
      )}
    </Panel>
  );
}
