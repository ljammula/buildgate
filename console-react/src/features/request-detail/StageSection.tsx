import { Loader2 } from "lucide-react";

import type { RequestSummary } from "@/domain/request";
import { OracleDraftingSection } from "@/shared/oracle/OracleDraftingSection";

import { Panel } from "./Panel";
import { RecoveryCallout } from "./RecoveryCallout";
import { ResumeCallout } from "./ResumeCallout";
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
      <Loader2 aria-hidden="true" className="size-4 animate-spin" />
      The factory is working on this step; this page updates on its own.
    </p>
  );
}

/**
 * What the request's state puts above its documents: a recovery callout, the
 * review controls, the drafting notice. The one place that selects by state;
 * building, pr_review, done and cancelled have no section of their own (their
 * tickets and the pipeline are the content).
 */
export function StageSection({ request, dialogs, detailLoaded, anyEditorOpen }: StageSectionProps) {
  switch (request.state) {
    case "submitted":
    case "spec_drafting":
    case "planning":
      return <WorkingNotice />;
    case "oracle_drafting":
      return (
        <Panel title="Oracle drafting">
          <OracleDraftingSection request={request} />
        </Panel>
      );
    case "spec_review":
    case "oracle_review":
    case "plan_review":
      return (
        <ReviewSection
          request={request}
          dialogs={dialogs}
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
    default:
      return null;
  }
}
