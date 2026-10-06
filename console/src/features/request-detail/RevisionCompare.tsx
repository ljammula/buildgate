import { useState } from "react";

import { useRequestRevision, useRequestRevisions } from "@/api/requestQueries";
import { formatLocalTimestamp } from "@/domain/elapsed";
import { type RequestSummary, type RevisionSummary, rejectionStage } from "@/domain/request";
import { anchoredChanges, rejectionForRevision } from "@/domain/reviewAnchors";
import { DiffView } from "@/ui/DiffView";
import { ErrorCallout } from "@/ui/ErrorDisplay";
import { Select } from "@/ui/Input";
import { Spinner } from "@/ui/Feedback";

import { Panel } from "./Panel";
import { RejectionNote } from "./RejectionNote";
import { currentContentFor, revisionDiffText } from "./requestDetailLogic";

function revisionLabel(revision: RevisionSummary): string {
  return `Revision ${revision.index} — rejected by ${revision.by} at ${formatLocalTimestamp(revision.at)}`;
}

/**
 * What changed since the operator rejected this document: their note,
 * quoted, whether the drafter was given it, each anchored note against the
 * section or criterion it was written on, and the diff between the rejected
 * revision and the current text. The note is the selected revision's own
 * (the latest rejection of this stage until the revisions load).
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
  // An operator's own edit also leaves a revision (the text it replaced):
  // this panel compares with what was rejected, so those are not offered.
  const list = (revisions.data ?? []).filter((r) => r.kind !== "edit");
  const latest = list.reduce<number | null>(
    (best, r) => (best === null || r.index > best ? r.index : best),
    null,
  );
  const selected = picked ?? latest;
  const detail = useRequestRevision(request.id, selected);
  const summary = list.find((r) => r.index === selected) ?? null;
  const note =
    (summary === null ? null : rejectionForRevision(request.rejections, summary)) ??
    [...request.rejections].reverse().find((r) => rejectionStage(r) === request.state);
  // The feedback record and the rejected text belong to the revision the
  // note was written on: neither is shown beside another revision's note.
  const own =
    note !== undefined && summary !== null && rejectionForRevision([note], summary) !== null
      ? summary
      : null;

  return (
    <Panel title="Changes since you rejected">
      {note === undefined ? null : (
        <RejectionNote
          rejection={note}
          feedbackSupplied={own === null ? null : own.feedbackSupplied}
          changes={
            own !== null && detail.data !== undefined && detail.data.index === own.index
              ? anchoredChanges(note, detail.data.files, (path) => currentContentFor(request, path))
              : null
          }
        />
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
              <h3 className="text-sm font-semibold">Every change since that revision</h3>
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
