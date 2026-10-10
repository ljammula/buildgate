import type { RequestSummary } from "@/domain/request";
import { statusForToken, statusIconForToken, type StatusIcon } from "@/domain/status";
import { DigestedText } from "@/shared/request/DigestedText";
import { RelativeTime } from "@/ui/RelativeTime";
import { ElapsedText } from "@/ui/Time";
import { cn } from "@/ui/cn";
import { statusIcons } from "@/ui/statusIcons";

import {
  type PipelineStep,
  type StepStatus,
  isAutomatedActor,
  pipelineOutcome,
  pipelineSteps,
  stepShowsDetail,
} from "./requestDetailLogic";

/** Step glyphs follow the tone map: green done, amber waits on the operator, teal working, red failed. */
function glyphFor(
  status: StepStatus,
  waitsOnOperator: boolean,
): { icon: StatusIcon; text: string; word: string } {
  switch (status) {
    case "done":
      return { icon: "circle_check", text: "text-tone-success", word: "done" };
    case "current":
      return {
        icon: "circle_dot",
        text: waitsOnOperator ? "text-tone-warning" : "text-tone-info",
        word: "current",
      };
    case "pending":
      return { icon: "circle_dashed", text: "text-fg-subtle", word: "pending" };
    case "failed":
      return { icon: "circle_x", text: "text-tone-danger", word: "failed" };
    case "needsYou":
      return { icon: "hourglass", text: "text-tone-warning", word: "needs you" };
    case "stopped":
      return { icon: "circle_minus", text: "text-tone-danger", word: "stopped" };
  }
}

/** When a step happened and, for a person's decision, who made it. */
function StepWhen({ at, by }: { readonly at: string; readonly by: string }) {
  return (
    <p className="text-fg-muted text-xs">
      <RelativeTime value={at} />
      {isAutomatedActor(by) ? null : ` · ${by}`}
    </p>
  );
}

function StepView({ step, request }: { step: PipelineStep; request: RequestSummary }) {
  const glyph = glyphFor(step.status, statusForToken(request.state) === "needsHuman");
  const Icon = statusIcons[glyph.icon];
  const emphasize = step.status !== "done" && step.status !== "pending";
  const entry = step.entry;
  const detail = stepShowsDetail(step);
  return (
    <li
      data-testid={`pipeline-step-${step.step}`}
      data-status={step.status}
      className={cn("flex gap-2", detail ? "py-1.5" : "py-0.5")}
    >
      <Icon aria-hidden="true" className={cn("mt-0.5 size-4 shrink-0", glyph.text)} />
      <div className="min-w-0 text-sm">
        <p className={cn(emphasize ? "font-semibold" : "text-fg-muted")}>
          {step.label}
          <span className="sr-only"> ({glyph.word})</span>
        </p>
        {!detail ? null : step.showsBuildProgress ? (
          <p className="text-fg-muted text-xs">
            {request.ticketCount > 0
              ? `ticket ${request.ticketIndex}/${request.ticketCount} · `
              : ""}
            <ElapsedText since={request.enteredAt} />
          </p>
        ) : entry === null ? (
          <p className="text-fg-muted text-xs">
            <RelativeTime value={request.enteredAt} />
          </p>
        ) : (
          <>
            <StepWhen at={entry.at} by={entry.by} />
            {entry.reason === "" ? null : (
              // A halt reason can run to a paragraph: its first sentence here,
              // the whole text one click away (it is on the page already, in
              // the recovery callout, but only a stepper reader needs this).
              <div className="text-fg-subtle text-xs">
                <DigestedText text={entry.reason} max={120} fullLabel="Full reason" />
              </div>
            )}
          </>
        )}
      </div>
    </li>
  );
}

/**
 * "Where is my ask in the pipeline?": one row per step with a glyph. A
 * completed step is one line unless a person decided it; the current step, a
 * failure and a needs-you step carry their when and why. See pipelineSteps for
 * how a history maps onto the steps.
 */
export function PipelineStepper({ request }: { readonly request: RequestSummary }) {
  const steps = pipelineSteps(request);
  const outcome = pipelineOutcome(request);
  const OutcomeIcon = outcome === null ? null : statusIcons[statusIconForToken(outcome.state)];
  return (
    <>
      <ol aria-label="Pipeline steps" className="flex flex-col">
        {steps.map((step) => (
          <StepView key={step.step} step={step} request={request} />
        ))}
      </ol>
      {outcome === null ? null : (
        // A request that ended outside the pipeline says so: its steps alone
        // read as "stopped somewhere", with no sign it was cancelled.
        <div
          data-testid="pipeline-outcome"
          data-state={outcome.state}
          className="border-border mt-1 flex gap-2 border-t pt-2"
        >
          {OutcomeIcon === null ? null : (
            <OutcomeIcon aria-hidden="true" className="text-tone-danger mt-0.5 size-4 shrink-0" />
          )}
          <div className="min-w-0 text-sm">
            <p className="font-semibold">{outcome.label}</p>
            {outcome.entry === null ? null : (
              <StepWhen at={outcome.entry.at} by={outcome.entry.by} />
            )}
            {outcome.entry === null ||
            outcome.entry.reason === "" ||
            outcome.reasonShownAtStep ? null : (
              <div className="text-fg-subtle text-xs">
                <DigestedText text={outcome.entry.reason} max={120} fullLabel="Full reason" />
              </div>
            )}
          </div>
        </div>
      )}
    </>
  );
}
