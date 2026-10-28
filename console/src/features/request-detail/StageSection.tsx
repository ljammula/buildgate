import { Loader2 } from "lucide-react";

import type { RequestSummary } from "@/domain/request";

import { ReadBackPanel } from "./ReadBackPanel";
import { RecoveryCallout } from "./RecoveryCallout";
import { ResumeCallout } from "./ResumeCallout";
import { ReviewNotes } from "./ReviewNotes";
import { ReviewSection } from "./ReviewSection";
import type { RequestDialogs } from "./useRequestDialogs";

export interface StageSectionProps {
  readonly request: RequestSummary;
  readonly dialogs: RequestDialogs;
  readonly detailLoaded: boolean;
  readonly anyEditorOpen: boolean;
}

function WorkingNotice() {
  return (
    <p className="text-fg-muted flex items-center gap-2 text-sm">
      <Loader2 aria-hidden="true" className="size-4 animate-spin motion-reduce:animate-none" />
      The factory is working on this step; this page updates on its own.
    </p>
  );
}

/**
 * What the request's state puts above its documents: a recovery callout, the
 * review controls (and a handed-over spec's read-back), the drafting notice. The one place that selects by state;
 * pr_review, done and cancelled have no section of their own (their
 * tickets and the pipeline are the content). A build shows only the notes
 * on a plan the factory approved itself.
 */
export function StageSection({ request, dialogs, detailLoaded, anyEditorOpen }: StageSectionProps) {
  switch (request.state) {
    case "submitted":
    case "spec_drafting":
    case "planning":
      return <WorkingNotice />;
    case "spec_review":
      return (
        <>
          <ReviewSection
            request={request}
            detailLoaded={detailLoaded}
            anyEditorOpen={anyEditorOpen}
          />
          <ReadBackPanel request={request} onDraftChanged={dialogs.onAnswerDraftChanged} />
        </>
      );
    case "plan_review":
      return (
        <ReviewSection
          request={request}
          detailLoaded={detailLoaded}
          anyEditorOpen={anyEditorOpen}
        />
      );
    case "halted":
    case "quarantined":
      return (
        <RecoveryCallout
          request={request}
          acting={dialogs.acting}
          onRetry={() => {
            dialogs.open("retry");
          }}
          onCancel={() => {
            dialogs.open("cancel");
          }}
          onSendBack={() => {
            dialogs.open("sendBack");
          }}
        />
      );
    case "resume_review":
      return (
        <ResumeCallout
          request={request}
          acting={dialogs.acting}
          onResume={(from) => {
            dialogs.open(from === "round" ? "resumeRound" : "resumeScratch");
          }}
          onCancel={() => {
            dialogs.open("cancel");
          }}
        />
      );
    case "building":
      // Notes on a plan the factory approved itself; nothing for any other build.
      return <ReviewNotes request={request} />;
    default:
      return null;
  }
}
