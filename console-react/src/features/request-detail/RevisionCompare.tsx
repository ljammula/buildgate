import { useState } from "react";

import { useRequestRevision, useRequestRevisions } from "@/api/requestQueries";
import { formatLocalTimestamp } from "@/domain/elapsed";
import type { RequestSummary, RevisionSummary } from "@/domain/request";
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
 * Opt-in compare of the current document against a rejected revision of it.
 * Offered only when a rejection of THIS stage exists (the caller checks):
 * a request rejected at spec_review has no ticket revision at plan_review.
 * Revisions are fetched only once the switch is on; a sole revision is
 * selected for the operator.
 */
export function RevisionCompare({ request }: { readonly request: RequestSummary }) {
  const [enabled, setEnabled] = useState(false);
  const [picked, setPicked] = useState<number | null>(null);
  const revisions = useRequestRevisions(request.id, enabled);
  const list = revisions.data ?? [];
  // A lone prior revision needs no picking.
  const selected = picked ?? (list.length === 1 ? (list[0]?.index ?? null) : null);
  const detail = useRequestRevision(request.id, selected);

  return (
    <Panel title="Revisions">
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
            <div data-testid="revision-diff" className="max-h-[28rem] overflow-auto">
              <DiffView diff={revisionDiffText(request, detail.data)} />
            </div>
          )}
        </>
      )}
    </Panel>
  );
}
