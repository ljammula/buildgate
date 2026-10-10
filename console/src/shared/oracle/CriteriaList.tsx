import { CircleCheck, CircleX } from "lucide-react";

import type { OracleDraftCriterion } from "@/domain/request";
import { EscapedText } from "@/shared/oracle/EscapedText";

export interface CriteriaListProps {
  readonly criteria: readonly OracleDraftCriterion[];
}

/**
 * The per-criterion eligibility list: one line per criterion the drafter
 * judged, eligible or not, with its own reason, so a `none_eligible` outcome
 * comes with a receipt rather than only a free-text summary.
 */
export function CriteriaList({ criteria }: CriteriaListProps) {
  return (
    <div data-testid="oracle-draft-criteria" className="flex flex-col gap-1">
      <h3 className="text-sm font-semibold">Per-criterion verdict</h3>
      {criteria.map((c) => {
        const Icon = c.eligible ? CircleCheck : CircleX;
        return (
          <div key={c.number} className="flex items-start gap-1.5 text-sm">
            <Icon
              className={
                c.eligible ? "text-accent size-4 shrink-0" : "text-tone-danger size-4 shrink-0"
              }
              aria-hidden="true"
            />
            <EscapedText
              text={`${c.number}. ${c.eligible ? "Eligible" : "Not eligible"}${
                c.reason === "" ? "" : `: ${c.reason}`
              }`}
            />
          </div>
        );
      })}
    </div>
  );
}
