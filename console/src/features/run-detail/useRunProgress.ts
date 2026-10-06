import { useEffect, useState } from "react";

import { useApi } from "@/api/ApiProvider";
import { watchRunProgress } from "@/api/runs";
import type { ApiError } from "@/domain/apiError";
import type { ProgressEvent } from "@/domain/run";
import {
  addProgressEvent,
  emptyProgressFeed,
  type ProgressFeed,
} from "@/features/run-detail/progressEvents";

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
  const [state, setState] = useState<FeedState>({ id, feed: emptyProgressFeed, error: null });

  useEffect(
    () =>
      watchRunProgress(http, id, {
        // A value from run `id` replaces whatever a previous run left behind.
        onValue: (event) => {
          setState((s) => ({
            id,
            feed: addProgressEvent(s.id === id ? s.feed : emptyProgressFeed, event),
            error: s.id === id ? s.error : null,
          }));
        },
        onError: (error) => {
          setState((s) => ({ id, feed: s.id === id ? s.feed : emptyProgressFeed, error }));
        },
      }),
    [http, id],
  );

  // Another run's feed is never shown, even for the render before its first event.
  return state.id === id ? { events: state.feed.events, error: state.error } : noProgress;
}

interface FeedState {
  readonly id: string;
  readonly feed: ProgressFeed;
  readonly error: ApiError | null;
}

const noProgress: RunProgress = { events: [], error: null };
