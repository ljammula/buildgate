/**
 * A `structuralSharing` function for a query whose cache entry is also written
 * by an event stream: `merge` receives the cached value (undefined on the
 * first fetch) and the fetched one, and returns what to keep, so a response
 * that was in flight cannot overwrite a newer streamed record.
 *
 * TanStack types `structuralSharing` over `unknown`, although it only ever
 * passes the query's own data type. The one cast that bridges that is here;
 * callers stay typed through `T`.
 */
export function keepNewer<T>(
  merge: (cached: T | undefined, fetched: T) => T,
): (cached: unknown, fetched: unknown) => T {
  return (cached, fetched) => merge(cached as T | undefined, fetched as T);
}
