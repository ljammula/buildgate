import type { RequestSummary } from "@/domain/request";
import { ApproveDialog } from "@/shared/approval/ApproveDialog";
import { CancelDialog } from "@/shared/approval/CancelDialog";
import { RejectDialog } from "@/shared/approval/RejectDialog";
import { ResumeDialog } from "@/shared/approval/ResumeDialog";
import { RetryDialog } from "@/shared/approval/RetryDialog";
import { SendBackDialog } from "@/shared/approval/SendBackDialog";

import type { RequestDialogs as Dialogs } from "./useRequestDialogs";

/**
 * Every approval flow the page can open, at most one at a time. Each fixes
 * the record it was opened with (a redraft arriving while one is open is
 * refused by the server rather than approved unread) and shows the server's
 * error inside itself while staying open.
 */
export function RequestDialogs({
  request,
  dialogs,
}: {
  readonly request: RequestSummary;
  readonly dialogs: Dialogs;
}) {
  const kind = dialogs.active?.kind ?? null;
  const shared = { request, onOpenChange: dialogs.onOpenChange, onDone: dialogs.onDone };
  return (
    <>
      <ApproveDialog
        {...shared}
        open={kind === "approve"}
        costSummary={request.costSummary}
        expectedSha256={dialogs.active?.expected ?? null}
      />
      <RejectDialog {...shared} open={kind === "reject"} />
      <RetryDialog {...shared} open={kind === "retry"} />
      <CancelDialog {...shared} open={kind === "cancel"} />
      <SendBackDialog {...shared} open={kind === "sendBack"} />
      <ResumeDialog {...shared} from="round" open={kind === "resumeRound"} />
      <ResumeDialog {...shared} from="scratch" open={kind === "resumeScratch"} />
    </>
  );
}
