import { useRequest } from "@/api/requestQueries";

/**
 * The run's own request title, shown beside the raw ticket id rather than in
 * place of it. A nice-to-have receipt: nothing depends on it, so a failed
 * fetch renders nothing.
 */
export function LinkedRequestTitle({ requestId }: { requestId: string }) {
  const request = useRequest(requestId);
  return request.data ? <>{request.data.title}</> : null;
}
