import { Link } from "react-router";
import { Ban, RotateCcw, ShieldCheck, Undo2 } from "lucide-react";

import { useApi } from "@/api/ApiProvider";
import type { RequestSummary } from "@/domain/request";
import { runOverridePath } from "@/routes/paths";
import { Button } from "@/ui/Button";
import { Callout } from "@/ui/Feedback";
import { CopyableCommand } from "@/ui/CopyableCommand";

import { recoveryPlan } from "./requestDetailLogic";

export interface RecoveryCalloutProps {
  readonly request: RequestSummary;
  /** A dialog is open: every action waits. */
  readonly acting: boolean;
  readonly onRetry: () => void;
  readonly onCancel: () => void;
  readonly onSendBack: () => void;
}

/**
 * The halted/quarantined callout: a kind-specific explanation (the server's
 * next_action when it gave one), Retry and Cancel, Send back when the server
 * says it would accept one, the copyable CLI equivalent, and for a
 * quarantine a link to the ticket's run. Always shown for these states and
 * never "waiting on you" with nothing offered: without write access the
 * buttons are disabled, not hidden.
 */
export function RecoveryCallout({
  request,
  acting,
  onRetry,
  onCancel,
  onSendBack,
}: RecoveryCalloutProps) {
  const { canWrite } = useApi();
  const plan = recoveryPlan(request);
  const disabled = acting || !canWrite;
  const sendBack = plan.showSendBack ? (
    <Button
      variant={plan.sendBackIsPrimary ? "primary" : "secondary"}
      disabled={disabled}
      onClick={onSendBack}
    >
      <Undo2 aria-hidden="true" />
      {plan.sendBackLabel}
    </Button>
  ) : null;
  return (
    <Callout
      data-testid="recovery-callout"
      tone={plan.awaitingPullRequest ? "neutral" : "danger"}
      title={plan.headline}
    >
      <div className="flex flex-col gap-3">
        <p className="break-words whitespace-pre-wrap">{plan.explanation}</p>
        <div className="flex flex-wrap items-center gap-2">
          {plan.sendBackIsPrimary ? sendBack : null}
          <Button variant="primary" disabled={disabled} onClick={onRetry}>
            <RotateCcw aria-hidden="true" />
            {plan.retryLabel}
          </Button>
          <Button disabled={disabled} onClick={onCancel}>
            <Ban aria-hidden="true" />
            Cancel request
          </Button>
          {plan.sendBackIsPrimary ? null : sendBack}
        </div>
        <CopyableCommand command={plan.cliEquivalent} />
        {plan.overrideTicket === null ? null : (
          <div className="flex flex-col items-start gap-2">
            <p>
              Correcting the ticket&apos;s own run record (accepted vs. halted) is a separate,
              optional action -- it does not affect whether Retry above will work.
            </p>
            <Button asChild>
              <Link
                to={runOverridePath(
                  plan.overrideTicket.runId,
                  `Request ${request.id} ticket ${plan.overrideTicket.index} quarantined`,
                )}
              >
                <ShieldCheck aria-hidden="true" />
                Review the ticket&apos;s run override
              </Link>
            </Button>
          </div>
        )}
      </div>
    </Callout>
  );
}
