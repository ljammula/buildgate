import { Ban, Play, RotateCcw } from "lucide-react";

import { useApi } from "@/api/ApiProvider";
import type { RequestSummary } from "@/domain/request";
import { Button } from "@/ui/Button";
import { Callout } from "@/ui/Feedback";
import { REQUEST_VERBS } from "@/domain/status";

import { resumePlan } from "./requestDetailLogic";

export interface ResumeCalloutProps {
  readonly request: RequestSummary;
  readonly acting: boolean;
  readonly onResume: (from: "round" | "scratch") => void;
  readonly onCancel: () => void;
}

/**
 * The resume_review callout: the step a lost worker left behind, why, and
 * the decisions (Resume from the last round, Rebuild from scratch, Cancel).
 * Nothing reruns until the operator picks one. While the server recorded
 * refusal reasons, Resume is withheld: only a rebuild or cancel are
 * possible. A lost drafting or planning step has one "Rerun step".
 */
export function ResumeCallout({ request, acting, onResume, onCancel }: ResumeCalloutProps) {
  const { canWrite } = useApi();
  const plan = resumePlan(request);
  const disabled = acting || !canWrite;
  return (
    <Callout data-testid="resume-callout" tone="warning" title={plan.headline}>
      <div className="flex flex-col gap-3">
        {plan.prompt === "" ? null : (
          <p className="break-words whitespace-pre-wrap">{plan.prompt}</p>
        )}
        {plan.refused.length === 0 ? null : (
          <div>
            <p className="font-semibold">Resume is not possible:</p>
            <ul>
              {plan.refused.map((reason, i) => (
                <li key={i} className="break-words">{`- ${reason}`}</li>
              ))}
            </ul>
          </div>
        )}
        <div className="flex flex-wrap items-center gap-2">
          {plan.isBuild ? (
            <>
              {plan.refused.length === 0 ? (
                <Button
                  variant="primary"
                  disabled={disabled}
                  onClick={() => {
                    onResume("round");
                  }}
                >
                  <Play aria-hidden="true" />
                  Resume
                </Button>
              ) : null}
              <Button
                disabled={disabled}
                onClick={() => {
                  onResume("scratch");
                }}
              >
                <RotateCcw aria-hidden="true" />
                Rebuild from scratch
              </Button>
            </>
          ) : (
            <Button
              variant="primary"
              disabled={disabled}
              onClick={() => {
                onResume("round");
              }}
            >
              <RotateCcw aria-hidden="true" />
              Rerun step
            </Button>
          )}
          <Button disabled={disabled} onClick={onCancel}>
            <Ban aria-hidden="true" />
            {REQUEST_VERBS.cancel}
          </Button>
        </div>
      </div>
    </Callout>
  );
}
