import { RefreshCw } from "lucide-react";
import { useQueryClient } from "@tanstack/react-query";
import { useMemo } from "react";

import { queryKeys } from "@/api/queryKeys";
import { useRequestBoard } from "@/api/requestQueries";
import { Button } from "@/ui/Button";
import { EmptyState, Spinner } from "@/ui/Feedback";
import { ErrorCallout } from "@/ui/ErrorDisplay";
import { PageBody, PageHeader } from "@/ui/PageLayout";

import { TriageWorkspace } from "./TriageWorkspace";
import { isNonEmpty, triageRequests } from "./triageModel";

/**
 * Batch triage at /triage: every request that needs the operator, in one
 * keyboard-driven list (j/k move). A spec or plan review is decided here (a
 * approve, r reject) through the same confirm dialogs as the request page;
 * any other state links to its request page. Deliberately no
 * multi-select and no "approve all": batch navigation is fine, batch approval
 * would make the human gate a formality.
 */
export function TriageScreen() {
  const { query } = useRequestBoard();
  const client = useQueryClient();
  const data = query.data;
  const requests = useMemo(() => (data === undefined ? [] : triageRequests(data)), [data]);

  const refresh = (
    <Button
      variant="ghost"
      size="icon"
      aria-label="Refresh"
      onClick={() => {
        // The list and the focused detail (the only detail query on screen).
        void client.invalidateQueries({ queryKey: queryKeys.requests.all });
      }}
    >
      <RefreshCw aria-hidden="true" />
    </Button>
  );

  return (
    <>
      <PageHeader title="Triage" actions={refresh} />
      <PageBody>
        {data === undefined ? (
          query.error === null ? (
            <Spinner />
          ) : (
            <div data-testid="triage-load-error">
              <ErrorCallout error={query.error} />
            </div>
          )
        ) : isNonEmpty(requests) ? (
          <TriageWorkspace requests={requests} />
        ) : (
          <EmptyState title="Nothing needs you right now." />
        )}
      </PageBody>
    </>
  );
}
