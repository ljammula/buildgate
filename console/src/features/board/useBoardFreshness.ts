import { maxSseFailuresBeforeDisconnected } from "@/api/polling";
import type { ApiError } from "@/domain/apiError";

import { type BoardFreshness, boardFreshness } from "./boardModel";

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
