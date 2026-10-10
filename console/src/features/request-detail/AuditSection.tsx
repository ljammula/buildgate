import { stopText } from "@/domain/runStop";
import type { RequestSummary } from "@/domain/request";
import { EscapedText } from "@/shared/oracle/EscapedText";

import { EditHistory } from "./EditHistory";
import { Panel } from "./Panel";
import { RejectionHistory } from "./RejectionHistory";
import { approvedLine, terminalEntries, terminalEntryLine } from "./requestDetailLogic";

/**
 * Who approved the request and when, every time it was cancelled, halted or
 * quarantined (who, when, why), its rejection history and the edits an
 * operator saved in place. Hidden when there is none of them.
 */
export function AuditSection({ request }: { readonly request: RequestSummary }) {
  const stops = terminalEntries(request);
  if (
    request.approvedBy === "" &&
    request.rejections.length === 0 &&
    request.edits.length === 0 &&
    stops.length === 0
  ) {
    return null;
  }
  return (
    <Panel title="Audit">
      {request.approvedBy === "" ? null : (
        <p className="text-sm text-balance">
          {approvedLine(request.approvedBy, request.approvedAt)}
        </p>
      )}
      {stops.length === 0 ? null : (
        <ul data-testid="audit-stops" className="flex flex-col gap-2">
          {stops.map((entry, i) => (
            <li key={i} className="text-sm">
              <p>{terminalEntryLine(entry)}</p>
              {entry.reason === "" ? null : (
                <EscapedText
                  text={[stopText(entry.reason).summary, stopText(entry.reason).cause]
                    .filter((part) => part !== "")
                    .join("\n")}
                  className="text-fg-muted text-xs whitespace-pre-wrap"
                />
              )}
            </li>
          ))}
        </ul>
      )}
      {request.rejections.length === 0 ? null : (
        <RejectionHistory rejections={request.rejections} />
      )}
      {request.edits.length === 0 ? null : (
        <EditHistory edits={request.edits} rejections={request.rejections} />
      )}
    </Panel>
  );
}
