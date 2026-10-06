import type { RequestSummary } from "@/domain/request";
import { EscapedText } from "@/shared/oracle/EscapedText";
import { oracleDraftStatusLabel } from "@/shared/oracle/oracleDraftStatus";
import { Spinner } from "@/ui/Feedback";

export interface OracleDraftingSectionProps {
  /** The request in `oracle_drafting`; its recorded draft status is the previous pass's outcome. */
  readonly request: RequestSummary;
}

/**
 * Shown while a request is in oracle_drafting: the factory is working, and a
 * previous pass's outcome (after a rejection) is still on the record.
 */
export function OracleDraftingSection({ request }: OracleDraftingSectionProps) {
  return (
    <div data-testid="oracle-drafting-section" className="flex flex-col gap-2 text-sm">
      <div className="flex items-start gap-2">
        <Spinner label="Drafting" />
        <p>
          Drafting acceptance-test oracles from the approved spec. Nothing to do yet -- this request
          moves to Oracle review when drafting finishes; this page updates on its own.
        </p>
      </div>
      {request.oracleDraftStatus !== "" ? (
        <EscapedText
          text={`Previous pass: ${oracleDraftStatusLabel(request.oracleDraftStatus)}${
            request.oracleDraftDetail === "" ? "" : ` -- ${request.oracleDraftDetail}`
          }`}
        />
      ) : null}
    </div>
  );
}
