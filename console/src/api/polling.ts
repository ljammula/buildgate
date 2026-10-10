// Every refresh interval and stream threshold the data hooks use, in one
// place to tune. Milliseconds unless the name says otherwise.

/** The board's poll of `GET /requests`, as a fallback under the event stream. */
export const requestListRefreshMs = 5_000;

/** The run list's poll of `GET /runs` (there is no run-list stream). */
export const runListRefreshMs = 5_000;

/** The sidebar's "needs you" count: a poll of the request list, no stream. */
export const needsYouPollMs = 10_000;

/** Mission Control's poll of `GET /queue-run`: what the worker is on now. */
export const queueRunPollMs = 10_000;

/** Mission Control's poll of `GET /stats`: numbers that move once per finished run. */
export const statsRefreshMs = 60_000;

/** How long a query's data counts as fresh when its own options say nothing. */
export const defaultQueryStaleTimeMs = 2_000;

/**
 * Connection attempts that must end without the stream opening before the
 * board says "disconnected": one dropped connection that reconnects at once
 * is not an alarm.
 */
export const maxSseFailuresBeforeDisconnected = 3;

/** The first wait before an event stream reconnects; it doubles per attempt. */
export const defaultInitialBackoffMs = 1_000;

/** The longest wait between two reconnect attempts. */
export const defaultMaxBackoffMs = 30_000;
