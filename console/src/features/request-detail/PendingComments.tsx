import { X } from "lucide-react";

import type { RequestSummary } from "@/domain/request";
import { anchorPlace, clipQuote } from "@/domain/reviewAnchors";
import { IconButton } from "@/ui/IconButton";

import { Panel } from "./Panel";
import { isReviewState } from "./requestDetailLogic";
import { usePendingComments } from "@/shared/request/usePendingComments";

/**
 * The comments written on passages of the spec or plan and not sent yet: each
 * with its place, the passage it is on and the note, removable. "Request
 * changes" sends them together, and the redraft changes only the sections they
 * are on. Nothing when there are none.
 */
export function PendingComments({ request }: { readonly request: RequestSummary }) {
  const { comments, remove } = usePendingComments(request);
  if (!isReviewState(request.state) || comments.length === 0) return null;
  return (
    <Panel title={`Comments to send (${comments.length})`} testId="pending-comments">
      <ul aria-label="Pending comments" className="flex flex-col gap-2">
        {comments.map((comment, i) => (
          <li key={i} className="flex items-start gap-2 text-sm">
            <span className="min-w-0 flex-1 break-words">
              <span className="text-fg-muted font-mono text-xs">{anchorPlace(comment)}</span>
              {comment.quote === "" ? null : (
                <q className="text-fg-muted ml-1 text-xs">{clipQuote(comment.quote)}</q>
              )}
              {`: ${comment.note}`}
            </span>
            <IconButton
              label={`Remove comment ${i + 1}`}
              onClick={() => {
                remove(i);
              }}
            >
              <X aria-hidden="true" />
            </IconButton>
          </li>
        ))}
      </ul>
      <p className="text-fg-muted text-xs">
        Request changes sends them together. Only the sections they are on are redrafted.
      </p>
    </Panel>
  );
}
