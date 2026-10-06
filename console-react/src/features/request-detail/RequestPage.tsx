import { useState } from "react";

import { useApi } from "@/api/ApiProvider";
import type { RequestSummary } from "@/domain/request";
import { rejectionStage } from "@/domain/request";
import { PageBody } from "@/ui/PageLayout";

import { AuditSection } from "./AuditSection";
import { ContentSections } from "./ContentSections";
import { NextActionBanner } from "./NextActionBanner";
import { OracleSkipWarning } from "./OracleSkipWarning";
import { Panel } from "./Panel";
import { PipelineStepper } from "./PipelineStepper";
import { RequestDialogs } from "./RequestDialogs";
import { RequestFacts } from "./RequestFacts";
import { RequestHeader } from "./RequestHeader";
import { RevisionCompare } from "./RevisionCompare";
import { ReviewActions } from "./ReviewActions";
import { StageSection } from "./StageSection";
import { TicketsSection } from "./TicketsSection";
import {
  contentFiles,
  nextActionText,
  showsNextBanner,
  ticketsLeadContent,
} from "./requestDetailLogic";
import { useRequestDialogs } from "./useRequestDialogs";

export interface RequestPageProps {
  /** The record on screen. Every hash and every dialog is derived from this same object. */
  readonly request: RequestSummary;
  readonly detailLoaded: boolean;
  readonly refreshing: boolean;
  /** The last refresh's failure while this older record is still shown; null otherwise. */
  readonly refreshError: unknown;
  readonly onRefresh: () => void;
  /** Refetches the detail and returns the fresh record (409 conflict resolution). */
  readonly refetchRequest: () => Promise<RequestSummary>;
}

/**
 * A loaded request: header, the "Next" line, then a main column (the state's
 * own section, the oracle-skip warning, the revision compare, the spec or
 * plan files, the tickets) beside a side column of facts (request facts, the
 * pipeline, the audit trail). Once tickets build, they lead the main column.
 */
export function RequestPage({
  request,
  detailLoaded,
  refreshing,
  refreshError,
  onRefresh,
  refetchRequest,
}: RequestPageProps) {
  const { canWrite } = useApi();
  const dialogs = useRequestDialogs(request);
  const files = contentFiles(request, canWrite);
  const [editing, setEditing] = useState<string | null>(null);
  // An editor closes the moment its file stops being editable (the request
  // moved on, or a live update changed its state): the edit would be
  // refused, and Approve must not stay disabled for an editor nobody sees.
  const editorOpen = editing !== null && files.some((f) => f.id === editing && f.editable);
  if (editing !== null && !editorOpen) setEditing(null);

  const next = nextActionText(request);
  const leads = ticketsLeadContent(request);
  const tickets = request.tickets.length > 0 ? <TicketsSection request={request} /> : null;
  const comparable = request.rejections.some((r) => rejectionStage(r) === request.state);

  return (
    <>
      <RequestHeader
        request={request}
        refreshing={refreshing}
        onRefresh={onRefresh}
        actions={
          <ReviewActions
            request={request}
            dialogs={dialogs}
            detailLoaded={detailLoaded}
            anyEditorOpen={editorOpen}
          />
        }
      />
      <PageBody>
        {showsNextBanner(request) && next !== null && next !== "" ? (
          <NextActionBanner text={next} />
        ) : null}
        <div className="grid items-start gap-6 lg:grid-cols-[minmax(0,1fr)_22rem]">
          <div className="flex min-w-0 flex-col gap-6">
            <StageSection
              request={request}
              dialogs={dialogs}
              detailLoaded={detailLoaded}
              anyEditorOpen={editorOpen}
            />
            <OracleSkipWarning request={request} />
            {leads ? tickets : null}
            {comparable ? <RevisionCompare request={request} /> : null}
            <ContentSections
              request={request}
              files={files}
              editing={editing}
              setEditing={setEditing}
              refetchRequest={refetchRequest}
            />
            {leads ? null : tickets}
          </div>
          <div className="flex min-w-0 flex-col gap-6">
            <RequestFacts request={request} refreshError={refreshError} />
            <Panel title="Pipeline">
              <PipelineStepper request={request} />
            </Panel>
            <AuditSection request={request} />
          </div>
        </div>
      </PageBody>
      <RequestDialogs request={request} dialogs={dialogs} />
    </>
  );
}
