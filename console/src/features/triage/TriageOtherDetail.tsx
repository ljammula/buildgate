import { Link } from "react-router";

import type { RequestSummary } from "@/domain/request";
import { DigestedText } from "@/shared/request/DigestedText";
import { RequestStageChip } from "@/shared/request/RequestStageChip";
import { RequestStatusUnit } from "@/shared/request/RequestStatusUnit";
import { requestPath } from "@/routes/paths";
import { Button } from "@/ui/Button";
import { Kbd } from "@/ui/Kbd";

import { triageReason } from "./triageModel";

export interface TriageOtherDetailProps {
  readonly request: RequestSummary;
  readonly now: Date;
}

/**
 * A request that needs the operator but is not decided from this list: its
 * state, why it needs you, and the way to the request page where that
 * state's action lives (oracle files are approved there, hash by hash).
 */
export function TriageOtherDetail({ request, now }: TriageOtherDetailProps) {
  return (
    <div className="border-border flex min-w-0 flex-col gap-3 rounded-lg border p-4">
      <h2 className="text-fg text-sm font-semibold">
        {request.title !== "" ? request.title : request.id}
      </h2>
      <RequestStatusUnit request={request} now={now}>
        <RequestStageChip state={request.state} needsYou />
      </RequestStatusUnit>
      <div data-testid="triage-reason" className="text-fg text-sm">
        <DigestedText text={triageReason(request)} fullLabel="Full next step" />
      </div>
      <div data-testid="triage-decision-bar" className="flex flex-col gap-2">
        <div>
          <Button variant="primary" asChild>
            <Link to={requestPath(request.id)}>Open request</Link>
          </Button>
        </div>
        <p className="text-fg-subtle text-xs">
          This one is decided on the request page. Keyboard: <Kbd>j</Kbd>/<Kbd>k</Kbd> move.
        </p>
      </div>
    </div>
  );
}
