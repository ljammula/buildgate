import { compareTimestamps } from "@/domain/elapsed";
import type { RequestSummary } from "@/domain/request";
import { stateLabel } from "@/domain/status";

/**
 * Why a Request changes or Send back dialog may no longer send: the request
 * as it is now (`live`) is not in the stage the dialog was opened on
 * (`opened`). Undefined while it still is: the same state, entered at the
 * same instant. A redraft that comes back to the same review state has a new
 * `enteredAt`. Pass the result as the dialog's `blocked`.
 */
export function movedOnNotice(live: RequestSummary, opened: RequestSummary): string | undefined {
  const rest =
    "Nothing can be sent from this dialog. Copy what you typed and close it to see the request as it is now.";
  if (live.state !== opened.state) {
    return `This request has moved on to ${stateLabel(live.state)}. ${rest}`;
  }
  if (compareTimestamps(live.enteredAt, opened.enteredAt) !== 0) {
    return `This request has moved on: it was redrafted and is in ${stateLabel(live.state)} again, with text you have not seen here. ${rest}`;
  }
  return undefined;
}
