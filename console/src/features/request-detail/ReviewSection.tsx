import { useApi } from "@/api/ApiProvider";
import type { RequestSummary } from "@/domain/request";
import { Callout } from "@/ui/Feedback";
import { TextWithCode } from "@/ui/TextWithCode";

import { Panel } from "./Panel";
import { ReviewNotes, hasReviewNotes } from "./ReviewNotes";

export interface ReviewSectionProps {
  readonly request: RequestSummary;
  readonly detailLoaded: boolean;
  readonly anyEditorOpen: boolean;
}

/** The review notes: why Approve is unavailable and the factory's notes on the spec or plan. */
export function ReviewSection({ request, detailLoaded, anyEditorOpen }: ReviewSectionProps) {
  const { canWrite } = useApi();
  const hasContent = !canWrite || !detailLoaded || hasReviewNotes(request) || anyEditorOpen;
  // A card with nothing in it (a spec review the operator can already act on) is noise.
  if (!hasContent) return null;
  return (
    <Panel title="Review">
      {!canWrite ? (
        <Callout tone="neutral" title="Note">
          This console cannot write here: the server has writes disabled for this origin and no
          override token is configured for this console build. Ask the operator running{" "}
          <TextWithCode text="`factoryd serve`" /> to bind it to loopback (or configure an override
          token) to enable approve/reject from here.
        </Callout>
      ) : null}
      {canWrite && !detailLoaded ? (
        <p className="text-fg-muted text-sm">
          Loading the full spec/plan content before enabling approve/reject...
        </p>
      ) : null}
      <ReviewNotes request={request} />
      {anyEditorOpen ? (
        <p className="text-fg-muted text-sm">
          Approve/Request changes are disabled while an edit is open. Save or Cancel it first.
        </p>
      ) : null}
    </Panel>
  );
}
