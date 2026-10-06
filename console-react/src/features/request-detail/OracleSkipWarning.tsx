import type { RequestSummary } from "@/domain/request";
import { EscapedText } from "@/shared/oracle/EscapedText";
import { Callout } from "@/ui/Feedback";

/**
 * The server-recorded warning that oracle_review was approved with no oracle
 * files after a draft that was not a deliberate none_eligible: the request is
 * being built without the acceptance test the operator asked for.
 */
export function OracleSkipWarning({ request }: { readonly request: RequestSummary }) {
  if (request.oracleSkipWarning === "") return null;
  const detail = request.oracleDraftDetail === "" ? "" : ` (${request.oracleDraftDetail})`;
  return (
    <Callout tone="danger" data-testid="oracle-skip-warning">
      <EscapedText text={`Warning: ${request.oracleSkipWarning}${detail}`} />
    </Callout>
  );
}
