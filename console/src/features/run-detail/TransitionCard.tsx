import type { KillSwitchTransition } from "@/domain/release";
import { formatLocalTimestamp } from "@/domain/elapsed";
import { EvidenceCard } from "@/features/run-detail/EvidenceCard";

/** One recorded kill-switch transition: who, when and why. */
export function TransitionCard({ transition }: { transition: KillSwitchTransition }) {
  return (
    <EvidenceCard
      title={transition.engaged ? "Engaged" : "Disengaged"}
      lines={[
        `By: ${transition.by}`,
        `At: ${formatLocalTimestamp(transition.at)}`,
        `Reason: ${transition.reason}`,
      ]}
    />
  );
}
