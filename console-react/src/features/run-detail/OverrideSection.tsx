import { Loader2, ShieldAlert } from "lucide-react";
import { useState } from "react";
import { useLocation } from "react-router";

import { useApi } from "@/api/ApiProvider";
import { useOverrideRun } from "@/api/runQueries";
import { OverrideDialog } from "@/features/run-detail/OverrideDialog";
import { parseRunOverrideReason } from "@/routes/paths";
import { Button } from "@/ui/Button";
import { ErrorCallout } from "@/ui/ErrorDisplay";
import { DescriptionItem, DescriptionList } from "@/ui/DescriptionList";
import { Section } from "@/ui/PageLayout";

/**
 * "Operator override" for a quarantined run. The button is gated on the
 * override token specifically, not on `canWrite` (found in an adversarial
 * review, 2026-09-24): unlike the request-pipeline writes `canWrite` covers,
 * the server's override route never grants the loopback no-token relaxation,
 * so a button enabled on `canWrite` alone would let an operator click it and
 * get a 403 back with no token this console can supply.
 */
export function OverrideSection({ runId }: { runId: string }) {
  const { hasOverrideToken } = useApi();
  const override = useOverrideRun(runId);
  const [open, setOpen] = useState(false);
  const initialReason = parseRunOverrideReason(useLocation().search);
  return (
    <Section title="Operator override" card>
      <DescriptionList labelWidth="sm">
        <DescriptionItem label="Action">
          Move this quarantined run to an accepted or halted state.
        </DescriptionItem>
        {override.error ? (
          <DescriptionItem label="Error">
            <p className="mb-1">Could not override run.</p>
            <ErrorCallout error={override.error} />
          </DescriptionItem>
        ) : null}
      </DescriptionList>
      <div>
        <Button
          variant="primary"
          disabled={override.isPending || !hasOverrideToken}
          onClick={() => {
            setOpen(true);
          }}
        >
          {override.isPending ? (
            <Loader2 className="animate-spin" aria-hidden="true" />
          ) : (
            <ShieldAlert aria-hidden="true" />
          )}
          {override.isPending ? "Applying…" : "Override run"}
        </Button>
      </div>
      {open ? (
        <OverrideDialog
          open
          onOpenChange={setOpen}
          initialReason={initialReason}
          onApply={(values) => {
            setOpen(false);
            override.mutate(values);
          }}
        />
      ) : null}
    </Section>
  );
}
