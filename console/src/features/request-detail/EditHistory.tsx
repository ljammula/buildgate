import { formatLocalTimestamp } from "@/domain/elapsed";
import { type Rejection, type RequestEdit, editHandoff } from "@/domain/request";
import { DiffView } from "@/ui/DiffView";
import { Disclosure } from "@/ui/Disclosure";

export interface EditHistoryProps {
  readonly edits: readonly RequestEdit[];
  readonly rejections: readonly Rejection[];
}

function handoffLine(edit: RequestEdit, rejections: readonly Rejection[]): string {
  const rejection = editHandoff(edit, rejections);
  return rejection === null
    ? "Not sent to a drafter: nothing has redrafted this file since."
    : `In the feedback of the rejection of ${formatLocalTimestamp(rejection.at)}: a redraft from it is told to keep these lines.`;
}

/**
 * Every in-place edit an operator saved to the spec or a ticket: who, when,
 * which file, the changed lines, and whether a later rejection carries the
 * edit in its feedback (what a redraft from that rejection is given; whether
 * one ran is the pipeline's to say). The lines are operator and agent text: shown through
 * the diff view, as text.
 */
export function EditHistory({ edits, rejections }: EditHistoryProps) {
  return (
    <Disclosure
      bare
      headingLevel={null}
      title={`Edits in place (${edits.length})`}
      testId="edit-history"
    >
      <ul className="flex flex-col gap-3">
        {edits.map((edit, i) => (
          <li key={i} className="flex flex-col gap-1 text-sm">
            <p className="text-fg-muted">
              {`Edited by ${edit.by} at ${formatLocalTimestamp(edit.at)} · `}
              <span className="font-mono text-xs">{edit.path}</span>
            </p>
            <DiffView diff={edit.diff} className="max-h-60 overflow-auto" />
            {edit.diffTruncated ? (
              <p className="text-fg-muted text-xs">
                {`Not every changed line is listed; revision ${edit.revision} holds the whole text this edit replaced.`}
              </p>
            ) : null}
            <p className="text-fg-muted text-xs">{handoffLine(edit, rejections)}</p>
          </li>
        ))}
      </ul>
    </Disclosure>
  );
}
