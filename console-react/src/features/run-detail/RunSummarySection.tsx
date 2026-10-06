import { ExternalLink } from "lucide-react";

import { describeError } from "@/ui/ErrorDisplay";
import { formatModelUsageLine } from "@/domain/cost";
import type { Run } from "@/domain/run";
import { Field, Fields } from "@/features/run-detail/Fields";
import { Button } from "@/ui/Button";
import { CompactId } from "@/ui/CompactId";
import { Section } from "@/ui/PageLayout";
import { StatusChipForToken } from "@/ui/StatusChip";
import { LocalTimeText } from "@/ui/Time";

export interface RunSummarySectionProps {
  readonly run: Run;
  readonly streamError: unknown;
  readonly temporalUiUrl: string | null;
}

/** The "Run" section: state, id, timestamps, usage, live-update health and the Temporal UI link. */
export function RunSummarySection({ run, streamError, temporalUiUrl }: RunSummarySectionProps) {
  // Offered only when the server advertises a Temporal UI and this run
  // recorded its workflow id: either missing means there is nothing to link to.
  const temporalHref =
    temporalUiUrl !== null && temporalUiUrl !== "" && run.temporalWorkflowId !== ""
      ? `${temporalUiUrl}/namespaces/default/workflows/${encodeURIComponent(run.temporalWorkflowId)}`
      : null;
  return (
    <Section title="Run" card>
      <Fields className="grid-cols-[6.5rem_minmax(0,1fr)]">
        <Field label="State">
          <StatusChipForToken token={run.state} />
        </Field>
        <Field label="Run ID" mono>
          <CompactId value={run.id} max={28} label="run id" />
        </Field>
        <Field label="Created">
          <LocalTimeText value={run.createdAt} />
        </Field>
        <Field label="Updated">
          <LocalTimeText value={run.updatedAt} />
        </Field>
        {/* Model and tokens only: the operator explicitly does not want a dollar figure here. */}
        {run.byModel.length > 0 ? (
          <Field label="Usage">
            {run.byModel.map((m) => formatModelUsageLine(m, run.tokensComplete)).join(", ")}
          </Field>
        ) : null}
        {streamError !== null ? (
          <Field label="Live updates">Disconnected: {describeError(streamError).raw}</Field>
        ) : null}
      </Fields>
      {temporalHref !== null ? (
        <div>
          <Button asChild>
            <a href={temporalHref} target="_blank" rel="noopener noreferrer">
              <ExternalLink aria-hidden="true" />
              Open in Temporal UI
            </a>
          </Button>
        </div>
      ) : null}
    </Section>
  );
}
