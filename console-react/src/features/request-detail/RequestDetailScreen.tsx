import { useParams } from "react-router";

import { ErrorCallout } from "@/ui/ErrorDisplay";
import { Spinner } from "@/ui/Feedback";
import { PageBody } from "@/ui/PageLayout";

import { RequestHeader } from "./RequestHeader";
import { RequestPage } from "./RequestPage";
import { useLiveRequest } from "./useLiveRequest";

/**
 * The request page (`/requests/:id`): where an operator reads a spec or a
 * plan and approves it. The approval is bound by SHA-256 to exactly what is
 * displayed, so this screen only ever hands the page the record on screen.
 */
export function RequestDetailScreen() {
  const { id = "" } = useParams();
  const { query, detailLoaded } = useLiveRequest(id);
  const request = query.data;
  const refresh = () => {
    void query.refetch();
  };

  if (request === undefined) {
    return (
      <>
        <RequestHeader request={null} refreshing={query.isFetching} onRefresh={refresh} />
        <PageBody>
          {query.isError ? (
            <div className="flex max-w-xl flex-col items-start gap-3">
              <ErrorCallout error={query.error} />
            </div>
          ) : (
            <Spinner label="Loading the request" />
          )}
        </PageBody>
      </>
    );
  }

  return (
    <RequestPage
      request={request}
      detailLoaded={detailLoaded}
      refreshing={query.isFetching}
      refreshError={query.isError ? query.error : null}
      onRefresh={refresh}
      refetchRequest={async () => {
        const result = await query.refetch();
        if (result.isError) throw result.error;
        if (result.data === undefined) throw new Error("The request could not be reloaded.");
        return result.data;
      }}
    />
  );
}
