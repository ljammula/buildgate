import { stopText } from "@/domain/runStop";
import { Link } from "react-router";
import { Ban, FileSearch, RotateCcw, ShieldCheck, Undo2 } from "lucide-react";

import { useApi } from "@/api/ApiProvider";
import { digestText } from "@/domain/digest";
import type { RequestSummary } from "@/domain/request";
import { runOverridePath, runPath } from "@/routes/paths";
import { EscapedText } from "@/shared/oracle/EscapedText";
import { Button } from "@/ui/Button";
import { Callout } from "@/ui/Feedback";
import { CompactId } from "@/ui/CompactId";
import { CopyableCommand } from "@/ui/CopyableCommand";
import { Disclosure } from "@/ui/Disclosure";
import { REQUEST_VERBS } from "@/domain/status";

import { quarantinedTicket, recoveryPlan } from "./requestDetailLogic";

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
 * quarantine a link to the ticket's run. Reads cause (whole), evidence link,
 * the lead action, a one-line explanation, then one closed disclosure for the
 * rest. Always shown for these states and
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
  const stop = stopText(request.error);
  // A server sentence that names a command or a branch is what the operator
  // needs to act (merge the branch by hand): never cut it, and when it is the
  // only instruction (no cause box above) show it whole as well.
  const digest = digestText(plan.explanation);
  const needsWhole = digest.truncated && (plan.explanation.includes("`") || request.error === "");
  const explanation = needsWhole ? { head: plan.explanation, truncated: false } : digest;
  const disabled = acting || !canWrite;
  // The receipts of the run that stopped: its log, diff and gates.
  const evidence = quarantinedTicket(request);
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
        {/* The cause, here where the explanation refers to it and the actions
            are: it was once only in the side column, with "the cause named
            above" pointing at nothing (found dogfooding, 2026-10-05). */}
        {request.error === "" ? null : (
          <div
            data-testid="recovery-cause"
            className="border-border bg-surface-sunken text-fg flex flex-col gap-1.5 rounded-md border px-3 py-2 font-mono text-xs break-words whitespace-pre-wrap"
          >
            {/* The factory's summary, then the innermost cause (it usually
                names the fix); the wrapped workflow error stays a click away. */}
            <p>
              <EscapedText text={stop.summary} />
            </p>
            {stop.cause === "" ? null : (
              <p className="text-tone-danger">
                <EscapedText text={stop.cause} />
              </p>
            )}
            {stop.full === "" ? null : (
              <Disclosure title="Full error" headingLevel={null} bare>
                <p className="text-fg-muted mt-1">
                  <EscapedText text={stop.full} />
                </p>
              </Disclosure>
            )}
          </div>
        )}
        {evidence === null ? null : (
          <p
            data-testid="recovery-evidence"
            className="flex flex-wrap items-center gap-x-2 gap-y-1"
          >
            <Link
              to={runPath(evidence.runId)}
              className="text-accent inline-flex items-center gap-1.5 underline underline-offset-2"
            >
              <FileSearch aria-hidden="true" className="size-4" />
              {`Evidence: run log, diff and gates (ticket ${evidence.index})`}
            </Link>
            <CompactId
              value={evidence.runId}
              max={24}
              label="run id"
              className="text-fg-muted text-xs"
            />
          </p>
        )}
        <div className="flex flex-wrap items-center gap-2">
          {plan.sendBackIsPrimary ? sendBack : null}
          <Button variant="primary" disabled={disabled} onClick={onRetry}>
            <RotateCcw aria-hidden="true" />
            {plan.retryLabel}
          </Button>
          <Button disabled={disabled} onClick={onCancel}>
            <Ban aria-hidden="true" />
            {REQUEST_VERBS.cancel}
          </Button>
          {plan.sendBackIsPrimary ? null : sendBack}
        </div>
        <p
          data-testid="recovery-explanation"
          title={explanation.truncated ? plan.explanation : undefined}
          className="text-fg-muted text-xs break-words whitespace-pre-wrap"
        >
          {explanation.head}
        </p>
        {/* One closed place for every other way out: the whole explanation
            when it was cut, the terminal equivalent of the lead action, and
            the run record's own correction. */}
        <Disclosure
          bare
          title="Other ways to resolve this"
          headingLevel="h3"
          testId="recovery-more"
        >
          {explanation.truncated ? (
            <p className="break-words whitespace-pre-wrap">{plan.explanation}</p>
          ) : null}
          <CopyableCommand command={plan.cliEquivalent} />
          {plan.overrideTicket === null ? null : (
            <div className="flex flex-col items-start gap-2">
              <p>
                Correcting the ticket&apos;s own run record (accepted vs. halted) is a separate,
                optional action; it does not affect whether Retry above will work.
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
        </Disclosure>
      </div>
    </Callout>
  );
}
