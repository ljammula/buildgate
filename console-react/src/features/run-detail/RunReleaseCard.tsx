import { useRunRelease } from "@/api/runQueries";
import { statusForReleaseDecision } from "@/domain/status";
import { Field, Fields } from "@/features/run-detail/Fields";
import { Section } from "@/ui/PageLayout";
import { StatusChip } from "@/ui/StatusChip";

export interface RunReleaseCardProps {
  readonly runId: string;
  /** A decision is recorded only once a run is accepted: no other run is asked. */
  readonly accepted: boolean;
}

/**
 * The run's release verdict: the actual decision and its first reason when
 * one is recorded, the explanation of what the release view is for only when
 * there is none (or it could not be read: the release view says why).
 */
export function RunReleaseCard({ runId, accepted }: RunReleaseCardProps) {
  const release = useRunRelease(runId, accepted);
  const decision = release.data?.decision ?? null;
  return (
    <Section title="Release" card>
      <Fields className="grid-cols-[5.5rem_minmax(0,1fr)]">
        {decision === null ? (
          <Field label="Decision">
            The factory-owned release decision for this run, and the project kill switch it was
            evaluated against.
          </Field>
        ) : (
          <>
            <Field label="Decision">
              <StatusChip
                status={statusForReleaseDecision(decision.allowed)}
                label={decision.allowed ? "Allowed" : "Denied"}
              />
            </Field>
            {decision.reasons[0] === undefined ? null : (
              <Field label="Why">
                <span title={decision.reasons.join("\n")} className="line-clamp-3">
                  {decision.reasons[0]}
                </span>
              </Field>
            )}
          </>
        )}
      </Fields>
    </Section>
  );
}
