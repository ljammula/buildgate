import type { ComposePhase } from "@/domain/run";
import { composeServiceLine, composeSummary } from "@/domain/runSummary";
import { Disclosure } from "@/ui/Disclosure";

/** The sidecar services each phase launched, as one closed line. Nothing for a run with no compose data. */
export function ComposeBlock({ phases }: { readonly phases: readonly ComposePhase[] }) {
  if (phases.length === 0) return null;
  return (
    <Disclosure title="Compose services" summary={composeSummary(phases)} testId="compose-block">
      {phases.map((phase, i) => (
        <div
          key={`${phase.phase}-${i}`}
          className="flex flex-col gap-0.5 rounded-md border border-border bg-surface-sunken p-3"
        >
          <h3 className="text-sm font-semibold text-fg">
            {phase.phase === "" ? "run" : phase.phase}
          </h3>
          {phase.enabled ? (
            composeServiceLine(phase).map((line) => (
              <p key={line} className="text-xs break-words text-fg-muted">
                {line}
              </p>
            ))
          ) : (
            <p className="text-xs break-words text-fg-muted">
              {`Not launched: ${phase.disabledReason}`}
            </p>
          )}
        </div>
      ))}
    </Disclosure>
  );
}
