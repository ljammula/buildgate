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
 * keep. A request seen for the first time (the list load, a first frame) only
 * records its time: what was announced before this tab looked is not news.
 * An empty time raises nothing and keeps the recorded one, so a server that
 * clears it and sets it later is still compared against the old value.
 */
export function attend(seen: SeenNotifications, request: RequestSummary): Attention {
  const recorded = seen.get(request.id);
  if (recorded === undefined) {
    return { seen: new Map(seen).set(request.id, request.lastNotifiedAt), raise: false };
  }
  if (request.lastNotifiedAt === "") return { seen, raise: false };
  if (recorded !== "" && !isLater(request.lastNotifiedAt, recorded)) {
    return { seen, raise: false };
  }
  // A request seen with no notification and now having one is news too.
  return { seen: new Map(seen).set(request.id, request.lastNotifiedAt), raise: true };
}

export interface NotificationContent {
  readonly title: string;
  readonly body: string;
  /** Replaces a notification with the same tag instead of stacking a second one. */
  readonly tag: string;
}

const fallbackTitle = "Buildgate: a request needs you";

/** What the notification says: the ask, then the project and title of the request. */
export function notificationContent(request: RequestSummary): NotificationContent {
  const title = request.lastAsk === "" ? fallbackTitle : request.lastAsk;
  const body = request.project === "" ? request.title : `${request.project}: ${request.title}`;
  return { title, body, tag: `${request.id}:${request.state}` };
}
