import { CircleAlert, CircleCheck } from "lucide-react";

import type { Run } from "@/domain/run";
import { runVerdictFailed, runVerdictLine } from "@/domain/runSummary";
import { cn } from "@/ui/cn";

/**
 * "Did it pass", in one line, for a run that is over. A halt or a failed gate
 * draws it in the failure colour; an accepted run stays quiet.
 */
export function RunVerdictLine({ run }: { readonly run: Run }) {
  const failed = runVerdictFailed(run);
  const Icon = failed ? CircleAlert : CircleCheck;
  return (
    <p
      data-testid="run-verdict"
      data-failed={failed}
      className={cn(
        "flex items-center gap-2 rounded-lg border px-4 py-3 text-sm font-medium",
        failed
          ? "border-tone-danger-border bg-tone-danger-soft text-tone-danger"
          : "border-border bg-surface text-fg",
      )}
    >
      <Icon
        aria-hidden="true"
        className={cn("size-4 shrink-0", failed ? "text-tone-danger" : "text-tone-success")}
      />
      {runVerdictLine(run)}
    </p>
  );
}
