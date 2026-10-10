// When a tab should raise a browser notification. Pure: the browser calls
// live in platform/browserNotifications.

import type { RequestSummary } from "@/domain/request";

/** Per request id, the newest `lastNotifiedAt` this tab has seen. */
export type SeenNotifications = ReadonlyMap<string, string>;

export interface Attention {
  readonly seen: SeenNotifications;
  /** True when this update is a new notification the operator should be told of. */
  readonly raise: boolean;
}

/** Whether `candidate` is a parsable time strictly after `recorded`; an unparsable one never is. */
function isLater(candidate: string, recorded: string): boolean {
  const next = Date.parse(candidate);
  if (Number.isNaN(next)) return false;
  const previous = Date.parse(recorded);
  return Number.isNaN(previous) || next > previous;
}

/**
 * Decides whether `request` raises a notification, and returns the map to
 * keep. `watchingSince` is the time (ms) from which this tab answers for the
 * notifications: nothing sent before it raises, whether the request is seen
 * for the first time or not. That is what keeps a tab from replaying what
 * was announced before it looked, and a tab that takes over as the notifier
 * from replaying what the one before it raised. An empty time raises nothing
 * and keeps the recorded one, so a server that clears it and sets it later
 * is still compared against the old value.
 */
export function attend(
  seen: SeenNotifications,
  request: RequestSummary,
  watchingSince: number,
): Attention {
  const recorded = seen.get(request.id);
  const at = Date.parse(request.lastNotifiedAt);
  const sinceWatching = !Number.isNaN(at) && at > watchingSince;
  if (recorded === undefined) {
    return {
      seen: new Map(seen).set(request.id, request.lastNotifiedAt),
      raise: sinceWatching,
    };
  }
  if (request.lastNotifiedAt === "") return { seen, raise: false };
  if (recorded !== "" && !isLater(request.lastNotifiedAt, recorded)) {
    return { seen, raise: false };
  }
  return {
    seen: new Map(seen).set(request.id, request.lastNotifiedAt),
    raise: sinceWatching,
  };
}

export interface NotificationContent {
  readonly title: string;
  readonly body: string;
  /**
   * One per notification the server sent: a second tab raising the same one
   * replaces it, and a later reminder for the same request alerts again.
   */
  readonly tag: string;
}

const fallbackTitle = "Buildgate: a request needs you";

/** What the notification says: the ask, then the project and title of the request. */
export function notificationContent(request: RequestSummary): NotificationContent {
  const title = request.lastAsk === "" ? fallbackTitle : request.lastAsk;
  const body = request.project === "" ? request.title : `${request.project}: ${request.title}`;
  return { title, body, tag: `${request.id}:${request.lastNotifiedAt}` };
}
