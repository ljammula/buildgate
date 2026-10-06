import { formatLocalTimestamp } from "@/domain/elapsed";
import type { Rejection } from "@/domain/request";
import type { AnchoredChange } from "@/domain/reviewAnchors";
import { anchorPlace } from "@/domain/reviewAnchors";
import { EscapedText } from "@/shared/oracle/EscapedText";
import { TextDiffView } from "@/ui/DiffView";

export interface RejectionNoteProps {
  readonly rejection: Rejection;
  /**
   * Whether the server recorded handing the note to the redraft; null while
   * the revision that says so is not loaded.
   */
  readonly feedbackSupplied: boolean | null;
  /**
   * Each anchored note against its place then and now; null while the
   * rejected revision's files are not loaded (the notes are still listed).
   */
  readonly changes: readonly AnchoredChange[] | null;
}

function AnchoredChangeBody({ change }: { readonly change: AnchoredChange }) {
  if (change.before === null && change.after === null) return null;
  if (change.after === null) {
    return <p className="text-fg-muted text-xs">This place is no longer in the current text.</p>;
  }
  if (change.before === null) {
    return (
      <p className="text-fg-muted text-xs">This place was not in the revision you rejected.</p>
    );
  }
  if (change.before === change.after) {
    return (
      <p className="text-tone-warning text-xs">
        Unchanged since you rejected: the text is the same.
      </p>
    );
  }
  return (
    <TextDiffView
      before={change.before}
      after={change.after}
      beforeLabel="rejected"
      afterLabel="current"
      className="max-h-64 overflow-auto"
    />
  );
}

/**
 * What the operator asked for when they rejected: the free note quoted,
 * whether the drafter was given it, and each anchored note beside the
 * section or criterion it was written against, as it was and as it is now.
 * All of it is the operator's and the agent's text: rendered as text.
 */
export function RejectionNote({ rejection, feedbackSupplied, changes }: RejectionNoteProps) {
  const anchored = rejection.anchors.length > 0;
  const free = anchored ? rejection.note : rejection.reason;
  return (
    <div className="flex flex-col gap-3">
      <blockquote
        data-testid="revision-note"
        className="border-border flex flex-col gap-1 border-l-2 pl-3 text-sm"
      >
        <p className="text-fg-muted text-xs">{`You asked (${rejection.by}, ${formatLocalTimestamp(rejection.at)}):`}</p>
        {free === "" ? null : <EscapedText text={free} />}
        {feedbackSupplied === null ? null : (
          <p
            data-testid="revision-feedback"
            className={feedbackSupplied ? "text-fg-muted text-xs" : "text-tone-warning text-xs"}
          >
            {feedbackSupplied
              ? "The drafter was given this note for the redraft."
              : "The drafter was not given this note: no hand-off is recorded for this revision, so read the redraft as written without it."}
          </p>
        )}
      </blockquote>
      {!anchored ? null : (
        <ol aria-label="Your notes on specific places" className="flex flex-col gap-3">
          {rejection.anchors.map((anchor, i) => {
            const change = changes?.[i];
            return (
              <li key={i} className="flex flex-col gap-1 text-sm">
                <p className="text-fg-muted font-mono text-xs break-words">{anchorPlace(anchor)}</p>
                <EscapedText text={anchor.note} />
                {change === undefined ? null : <AnchoredChangeBody change={change} />}
              </li>
            );
          })}
        </ol>
      )}
    </div>
  );
}
