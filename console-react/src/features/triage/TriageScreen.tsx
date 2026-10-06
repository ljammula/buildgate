import { RefreshCw } from "lucide-react";
import { useMemo, useState } from "react";

import { useRequestBoard } from "@/api/requestQueries";
import { Button } from "@/ui/Button";
import { EmptyState, Spinner } from "@/ui/Feedback";
import { ErrorCallout } from "@/ui/ErrorDisplay";
import { PageBody, PageHeader } from "@/ui/PageLayout";

import { TriageWorkspace } from "./TriageWorkspace";
import { isNonEmpty, triageRequests } from "./triageModel";

/**
 * Batch triage at /triage: the requests waiting on a spec or plan decision in
 * one keyboard-driven list (j/k move, a approve, r reject), each decided
 * through the same confirm dialogs as the request page. Deliberately no
 * multi-select and no "approve all": batch navigation is fine, batch approval
 * would make the human gate a formality.
 */
export function TriageScreen() {
  const { query } = useRequestBoard();
  const [refreshes, setRefreshes] = useState(0);
  const data = query.data;
  const requests = useMemo(() => (data === undefined ? [] : triageRequests(data)), [data]);

  const refresh = (
    <Button
      variant="ghost"
      size="icon"
      aria-label="Refresh"
      onClick={() => {
        void query.refetch();
        setRefreshes((n) => n + 1);
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
          <TriageWorkspace requests={requests} refreshes={refreshes} />
        ) : (
          <EmptyState title="Nothing needs you right now." />
        )}
      </PageBody>
    </>
  );
}
