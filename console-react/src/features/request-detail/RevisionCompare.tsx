import { useState } from "react";

import { useRequestRevision, useRequestRevisions } from "@/api/requestQueries";
import { formatLocalTimestamp } from "@/domain/elapsed";
import { type RequestSummary, type RevisionSummary, rejectionStage } from "@/domain/request";
import { EscapedText } from "@/shared/oracle/EscapedText";
import { DiffView } from "@/ui/DiffView";
import { ErrorCallout } from "@/ui/ErrorDisplay";
import { Select } from "@/ui/Input";
import { Spinner } from "@/ui/Feedback";

import { Panel } from "./Panel";
import { revisionDiffText } from "./requestDetailLogic";

function revisionLabel(revision: RevisionSummary): string {
  return `Revision ${revision.index} — rejected by ${revision.by} at ${formatLocalTimestamp(revision.at)}`;
}

/**
 * What changed since the operator rejected this document: their last note,
 * quoted, and the diff between the rejected revision and the current text.
 * Open by default (the switch turns it off): at a re-review this is the
 * question, and the full current text stays whole below it, because the
 * approval is bound to that text and not to the diff. Offered only when a
 * rejection of THIS stage exists (the caller checks): a request rejected at
 * spec_review has no ticket revision at plan_review. The latest revision is
 * selected unless the operator picks another.
 */
export function RevisionCompare({ request }: { readonly request: RequestSummary }) {
  const [enabled, setEnabled] = useState(true);
  const [picked, setPicked] = useState<number | null>(null);
  const revisions = useRequestRevisions(request.id, enabled);
  const list = revisions.data ?? [];
  const latest = list.reduce<number | null>(
    (best, r) => (best === null || r.index > best ? r.index : best),
    null,
  );
  const selected = picked ?? latest;
  const detail = useRequestRevision(request.id, selected);
  const note = [...request.rejections].reverse().find((r) => rejectionStage(r) === request.state);

  return (
    <Panel title="Revisions">
      {note === undefined ? null : (
        <blockquote
          data-testid="revision-note"
          className="border-border flex flex-col gap-1 border-l-2 pl-3 text-sm"
        >
          <p className="text-fg-muted text-xs">{`You asked (${note.by}, ${formatLocalTimestamp(note.at)}):`}</p>
          <EscapedText text={note.reason} />
        </blockquote>
      )}
      <label className="flex items-center gap-2 text-sm">
        <input
          type="checkbox"
          role="switch"
          checked={enabled}
          onChange={(event) => {
            setEnabled(event.target.checked);
          }}
        />
        Compare with rejected revision
      </label>
      {!enabled ? null : revisions.isPending ? (
        <Spinner label="Loading revisions" />
      ) : revisions.isError ? (
        <div role="alert" className="text-sm">
          <p className="text-tone-danger">Could not load revisions:</p>
          <ErrorCallout error={revisions.error} />
        </div>
      ) : list.length === 0 ? (
        <p className="text-sm">No rejected revisions recorded.</p>
      ) : (
        <>
          <Select
            aria-label="Revision"
            value={selected === null ? "" : String(selected)}
            onChange={(event) => {
              setPicked(event.target.value === "" ? null : Number(event.target.value));
            }}
          >
            <option value="">Select a revision</option>
            {list.map((revision) => (
              <option key={revision.index} value={String(revision.index)}>
                {revisionLabel(revision)}
              </option>
            ))}
          </Select>
          {selected === null ? null : detail.isPending ? (
            <Spinner label="Loading the revision" />
          ) : detail.isError ? (
            <div role="alert" className="text-sm">
              <p className="text-tone-danger">Could not load revision:</p>
              <ErrorCallout error={detail.error} />
            </div>
          ) : (
            <>
              <h3 className="text-sm font-semibold">Changes since you rejected</h3>
              <div data-testid="revision-diff" className="max-h-[28rem] overflow-auto">
                <DiffView diff={revisionDiffText(request, detail.data)} />
              </div>
            </>
          )}
        </>
      )}
    </Panel>
  );
}
