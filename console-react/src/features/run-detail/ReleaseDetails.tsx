import type { ReleaseView } from "@/domain/release";
import { statusForReleaseDecision } from "@/domain/status";
import { TransitionCard } from "@/features/run-detail/TransitionCard";
import { Badge } from "@/ui/Badge";
import { DescriptionItem, DescriptionList } from "@/ui/DescriptionList";
import { Section } from "@/ui/PageLayout";
import { KillSwitchChip, StatusChip } from "@/ui/StatusChip";
import { LocalTimeText } from "@/ui/Time";

/**
 * What the factory decided about releasing one run, why, and the project kill
 * switch it was evaluated against. Strictly observability: nothing here acts
 * on an allowed decision (no merge, push or deploy is wired to one anywhere),
 * and there is deliberately no control to engage or disengage the kill switch:
 * that stays CLI-only so reaching for it never depends on a healthy
 * `factoryd serve` (see CLAIMS.md).
 */
export function ReleaseDetails({ release }: { release: ReleaseView }) {
  const { decision, recordingFailure, killSwitch } = release;
  return (
    <>
      <Section title="Release decision">
        <DescriptionList>
          <DescriptionItem label="Project">{release.project}</DescriptionItem>
          {recordingFailure !== null ? (
            // Evaluated, but the decision could not be durably recorded (an
            // unreadable kill-switch.json, say): distinct from "no decision
            // recorded", which means recording was never attempted.
            <>
              <DescriptionItem label="Decision">
                <Badge tone="danger">
                  release decision could not be recorded: {recordingFailure.error} -- fix and
                  `factoryd retry`
                </Badge>
              </DescriptionItem>
              <DescriptionItem label="Failed at">
                <LocalTimeText value={recordingFailure.at} />
              </DescriptionItem>
            </>
          ) : decision === null ? (
            // Absence of a decision is never an allowed one: nothing records
            // one until a run is accepted.
            <>
              <DescriptionItem label="Decision">
                <Badge tone="neutral" variant="outline">
                  No decision recorded
                </Badge>
              </DescriptionItem>
              <DescriptionItem label="Why">
                No release decision has been recorded for this run. One is recorded only when a run
                is accepted.
              </DescriptionItem>
            </>
          ) : (
            <>
              <DescriptionItem label="Decision">
                <StatusChip
                  status={statusForReleaseDecision(decision.allowed)}
                  label={decision.allowed ? "Allowed" : "Denied"}
                />
              </DescriptionItem>
              <DescriptionItem label="Run ID" mono>
                {decision.runId}
              </DescriptionItem>
              <DescriptionItem label="Evaluated">{decision.evaluatedAt}</DescriptionItem>
              {decision.reasons.length === 0 ? (
                <DescriptionItem label="Reasons">None recorded</DescriptionItem>
              ) : (
                decision.reasons.map((reason, i) => (
                  <DescriptionItem key={i} label="Reason">
                    {reason}
                  </DescriptionItem>
                ))
              )}
            </>
          )}
          {/* Stated on the page itself: an operator reading "Allowed" must not infer that anything then happens. */}
          <DescriptionItem label="Effect">
            Recorded evidence only. No merge, push, or deploy is performed from this decision.
          </DescriptionItem>
        </DescriptionList>
      </Section>
      <Section title="Project kill switch">
        <DescriptionList>
          <DescriptionItem label="State">
            <KillSwitchChip engaged={killSwitch.engaged} />
          </DescriptionItem>
          <DescriptionItem label="Control">
            Engage and disengage from the command line (factoryd kill-switch). It is deliberately
            not a console action, so hitting it never depends on a healthy factoryd serve.
          </DescriptionItem>
          {killSwitch.history.length === 0 ? (
            <DescriptionItem label="History">
              Never engaged. This project has no recorded transitions.
            </DescriptionItem>
          ) : null}
        </DescriptionList>
        {killSwitch.history.map((transition, i) => (
          <TransitionCard key={i} transition={transition} />
        ))}
      </Section>
    </>
  );
}
