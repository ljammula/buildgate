import type { ApiError } from "@/domain/apiError";

import { type BoardFreshness, boardFreshness } from "./boardModel";

/**
 * How many connection attempts may end, since the stream was last open,
 * before the indicator calls it an outage rather than a blip.
 */
export const maxSseFailuresBeforeDisconnected = 3;

/**
 * The indicator state for the board's event stream: live while connected,
 * "recent" while reconnecting, "disconnected" after a permanent failure or
 * maxSseFailuresBeforeDisconnected failed attempts. Never a reason to stop
 * the 5 s poll, which keeps running regardless.
 */
export function useBoardFreshness(
  live: boolean,
  streamError: ApiError | null,
  failedAttempts: number,
): BoardFreshness {
  return boardFreshness({
    live,
    disconnected: streamError !== null || failedAttempts >= maxSseFailuresBeforeDisconnected,
  });
}
