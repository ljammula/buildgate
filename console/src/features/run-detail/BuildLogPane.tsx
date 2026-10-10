import { Checkbox } from "@/ui/Input";
import { useId } from "react";

import { useRunLog } from "@/features/run-detail/useRunLog";
import { ErrorCallout } from "@/ui/ErrorDisplay";
import { LogView } from "@/ui/CodeBlock";

export interface BuildLogPaneProps {
  readonly runId: string;
  readonly enabled: boolean;
  readonly onEnabledChange: (enabled: boolean) => void;
}

/**
 * The collapsible live build log: off by default and opt-in per run. A
 * viewer only: no input is ever sent from here. The log is worker-authored,
 * untrusted text, shown as text with no link detection.
 */
export function BuildLogPane({ runId, enabled, onEnabledChange }: BuildLogPaneProps) {
  const hintId = useId();
  const log = useRunLog(runId, enabled);
  return (
    <div className="flex flex-col gap-2">
      <label className="flex items-start gap-2 text-sm">
        <Checkbox
          role="switch"
          checked={enabled}
          aria-describedby={hintId}
          className="mt-0.5"
          onChange={(e) => {
            onEnabledChange(e.target.checked);
          }}
        />
        <span className="flex flex-col">
          <span className="text-fg">Show live build log</span>
          <span id={hintId} className="text-xs text-fg-muted">
            Viewer only: plain text, and no input is ever sent from here.
          </span>
        </span>
      </label>
      {enabled ? (
        <>
          {log.error !== null ? <ErrorCallout error={log.error} onRetry={log.retry} /> : null}
          {log.closed ? <p className="text-sm text-fg-muted">Log stream closed.</p> : null}
          <LogView text={log.text} />
        </>
      ) : null}
    </div>
  );
}
