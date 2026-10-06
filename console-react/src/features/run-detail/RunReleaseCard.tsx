import { useRunRelease } from "@/api/runQueries";
import { statusForReleaseDecision } from "@/domain/status";
import { DescriptionItem, DescriptionList } from "@/ui/DescriptionList";
import { Section } from "@/ui/PageLayout";
import { StatusChip } from "@/ui/StatusChip";

export interface RunReleaseCardProps {
  readonly runId: string;
  /** A decision is recorded only once a run is accepted: no other run is asked. */
  readonly accepted: boolean;
}

/**
 * The run's release verdict: the actual decision and its first reason when
 * one is recorded; "Not evaluated yet" when there is none (a decision is
 * recorded only once a run is accepted), or why it could not be read.
 */
export function RunReleaseCard({ runId, accepted }: RunReleaseCardProps) {
  const release = useRunRelease(runId, accepted);
  const decision = release.data?.decision ?? null;
  return (
    <Section title="Release" card>
      <DescriptionList labelWidth="sm">
        {decision === null ? (
          <DescriptionItem label="Decision">
            {release.isLoading
              ? "Loading…"
              : release.isError
                ? "Could not be read"
                : "Not evaluated yet"}
          </DescriptionItem>
        ) : (
          <>
            <DescriptionItem label="Decision">
              <StatusChip
                status={statusForReleaseDecision(decision.allowed)}
                label={decision.allowed ? "Allowed" : "Denied"}
              />
            </DescriptionItem>
            {decision.reasons[0] === undefined ? null : (
              <DescriptionItem label="Why">
                <span title={decision.reasons.join("\n")} className="line-clamp-3">
                  {decision.reasons[0]}
                </span>
              </DescriptionItem>
            )}
          </>
        )}
      </DescriptionList>
    </Section>
  );
}
