import { useEffect, useReducer, useState } from "react";

import { useApi } from "@/api/ApiProvider";
import { watchRunProgress } from "@/api/runs";
import type { ApiError } from "@/domain/apiError";
import type { ProgressEvent } from "@/domain/run";
import { addProgressEvent, emptyProgressFeed } from "@/features/run-detail/progressEvents";

export interface RunProgress {
  readonly events: readonly ProgressEvent[];
  /** A permanent failure of the feed (a 4xx); transient drops are retried silently. */
  readonly error: ApiError | null;
}

/**
 * A run's progress feed, subscribed whether or not the run is terminal: the
 * server replays a finished run's whole recorded history before closing, so
 * no separate one-shot fetch is needed. The server also replays every
 * existing line on each reconnect, which `addProgressEvent` de-duplicates.
 */
export function useRunProgress(id: string): RunProgress {
  const { http } = useApi();
  const [feed, add] = useReducer(addProgressEvent, emptyProgressFeed);
  const [error, setError] = useState<ApiError | null>(null);

  useEffect(() => watchRunProgress(http, id, { onValue: add, onError: setError }), [http, id]);

  return { events: feed.events, error };
}
