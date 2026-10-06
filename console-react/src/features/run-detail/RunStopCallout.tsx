import type { Run } from "@/domain/run";
import { runStop } from "@/domain/runStop";
import { EscapedText } from "@/shared/oracle/EscapedText";
import { Disclosure } from "@/ui/Disclosure";
import { Callout } from "@/ui/Feedback";

export interface RunStopCalloutProps {
  readonly run: Run;
}

/**
 * Why a halted or quarantined run stopped: the server's innermost cause in
 * full (it usually names the fix), its code, the factory's triage sentence,
 * and the whole recorded error one click away. Nothing for a run that did
 * not stop this way. The text is the server's and may quote agent output:
 * it is rendered as text.
 */
export function RunStopCallout({ run }: RunStopCalloutProps) {
  const stop = runStop(run);
  if (stop === null) return null;
  return (
    <Callout tone="danger" title="Why this run stopped" data-testid="run-stop">
      <div className="flex flex-col gap-2">
        {stop.triage === "" ? null : (
          <p className="break-words whitespace-pre-wrap">
            <EscapedText text={stop.triage} />
          </p>
        )}
        {stop.cause === "" ? null : (
          <p className="font-mono text-xs break-words whitespace-pre-wrap">
            <EscapedText text={stop.cause} />
          </p>
        )}
        {stop.code === "" ? null : (
          <p className="text-fg-muted text-xs">
            Reason code: <span className="font-mono">{stop.code}</span>
          </p>
        )}
        {stop.full === "" ? null : (
          <Disclosure title="Full error" headingLevel={null} bare>
            <pre className="bg-surface-sunken text-fg-muted mt-1 overflow-x-auto rounded-md p-2 text-xs whitespace-pre-wrap">
              <EscapedText text={stop.full} />
            </pre>
          </Disclosure>
        )}
      </div>
    </Callout>
  );
}
