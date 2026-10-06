import { CircleAlert, CircleCheck, CircleDashed, Hourglass, type LucideIcon } from "lucide-react";

import type { RequestSummary } from "@/domain/request";
import { ElapsedText, useNow } from "@/ui/Time";
import { cn } from "@/ui/cn";

import {
  type PipelineStep,
  type StepStatus,
  formatWhen,
  pipelineSteps,
} from "./requestDetailLogic";

const GLYPHS: Readonly<Record<StepStatus, { icon: LucideIcon; text: string; word: string }>> = {
  done: { icon: CircleCheck, text: "text-accent", word: "done" },
  current: { icon: CircleDashed, text: "text-accent", word: "current" },
  pending: { icon: CircleDashed, text: "text-fg-subtle", word: "pending" },
  failed: { icon: CircleAlert, text: "text-tone-danger", word: "failed" },
  needsYou: { icon: Hourglass, text: "text-tone-warning", word: "needs you" },
};

function StepView({
  step,
  request,
  now,
}: {
  step: PipelineStep;
  request: RequestSummary;
  now: Date;
}) {
  const glyph = GLYPHS[step.status];
  const Icon = glyph.icon;
  const emphasize = step.status !== "done" && step.status !== "pending";
  const entry = step.entry;
  return (
    <li
      data-testid={`pipeline-step-${step.step}`}
      data-status={step.status}
      className="flex gap-2 py-1.5"
    >
      <Icon aria-hidden="true" className={cn("mt-0.5 size-4 shrink-0", glyph.text)} />
      <div className="min-w-0 text-sm">
        <p className={cn(emphasize ? "font-semibold" : "text-fg-muted")}>
          {step.label}
          <span className="sr-only"> ({glyph.word})</span>
        </p>
        {step.showsBuildProgress ? (
          <p className="text-fg-muted text-xs">
            {request.ticketCount > 0
              ? `ticket ${request.ticketIndex}/${request.ticketCount} · `
              : ""}
            <ElapsedText since={request.enteredAt} />
          </p>
        ) : entry === null ? (
          emphasize ? (
            <p className="text-fg-muted text-xs">{formatWhen(request.enteredAt, now)}</p>
          ) : null
        ) : (
          <>
            <p className="text-fg-muted text-xs">{`${formatWhen(entry.at, now)} · ${entry.by}`}</p>
            {entry.reason === "" ? null : (
              // Capped: a halt reason can run to a paragraph, and the full
              // text is on the page already (recovery callout, Detail row).
              <p className="text-fg-subtle line-clamp-4 text-xs break-words" title={entry.reason}>
                {entry.reason}
              </p>
            )}
          </>
        )}
      </div>
    </li>
  );
}

/**
 * "Where is my ask in the pipeline?": one row per step with a glyph and the
 * history entry that reached it (when, by whom, why), never a bare state
 * word. See pipelineSteps for how a history maps onto the steps.
 */
export function PipelineStepper({ request }: { readonly request: RequestSummary }) {
  const now = useNow(60_000);
  const steps = pipelineSteps(request);
  return (
    <ol aria-label="Pipeline steps" className="flex flex-col">
      {steps.map((step) => (
        <StepView key={step.step} step={step} request={request} now={now} />
      ))}
    </ol>
  );
}
