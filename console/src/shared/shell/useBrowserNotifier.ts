import { useCallback, useEffect, useRef, useState } from "react";
import { useNavigate } from "react-router";

import { needsYouPollMs } from "@/api/polling";
import { useNotifierStream, useRequests } from "@/api/requestQueries";
import { type SeenNotifications, attend, notificationContent } from "@/domain/attention";
import type { RequestSummary } from "@/domain/request";
import {
  type NotificationSupport,
  getNotificationsPreference,
  holdNotifierLock,
  notificationSupport,
  recordWatching,
  requestNotificationPermission,
  setNotificationsPreference,
  showNotification,
  watchingSince,
} from "@/platform/browserNotifications";
import { requestPath } from "@/routes/paths";

/** How long the server holds the host's banner back after a notifying tab's stream ends. */
const takeoverSlackMs = 20_000;

export interface BrowserNotifier {
  readonly support: NotificationSupport;
  /** The operator turned notifications on in this browser. */
  readonly on: boolean;
  /** This tab raises them: allowed, turned on, and the only tab holding the lock. */
  readonly active: boolean;
  /** Asks for permission (from a click) and, once granted, turns them on. */
  readonly turnOn: () => void;
  readonly turnOff: () => void;
}

/**
 * Raises a browser notification when a request starts waiting on the
 * operator. While this tab is the one raising them it holds the notifier
 * stream on every screen (useNotifierStream), which is also what makes the
 * host's own banner stand down. Of several
 * tabs of this console only the one holding the notifier lock raises them; a
 * click on one focuses that tab and opens the request's page. Every tab
 * records what it has seen, so a tab that becomes the notifier does not
 * replay old asks.
 */
export function useBrowserNotifier(): BrowserNotifier {
  const [support, setSupport] = useState(notificationSupport);
  const [on, setOn] = useState(getNotificationsPreference);
  const [holding, setHolding] = useState(false);
  const wanted = support === "granted" && on;
  const active = wanted && holding;

  useEffect(() => {
    if (!wanted) return undefined;
    const release = holdNotifierLock(() => {
      setHolding(true);
    });
    return () => {
      release();
      setHolding(false);
    };
  }, [wanted]);

  const navigate = useNavigate();
  const { data } = useRequests(needsYouPollMs);
  const seen = useRef<SeenNotifications>(new Map());
  // From when this tab answers for the notifications: its last look (kept
  // across a reload), or, for a tab that takes over as the notifier, a
  // little before it did: the host's banner stays held back that long after
  // the tab before it went away.
  const [firstLook] = useState(() => watchingSince(Date.now()));
  const since = useRef(firstLook);
  useEffect(() => {
    if (active) since.current = Math.max(since.current, Date.now() - takeoverSlackMs);
  }, [active]);
  // The list the shell polls and the notifier stream's frames both pass
  // through here; `seen` is what keeps one ask from being raised twice.
  const consider = useCallback(
    (request: RequestSummary) => {
      const attention = attend(seen.current, request, since.current);
      seen.current = attention.seen;
      recordWatching(Date.now());
      if (attention.raise && active) {
        showNotification(notificationContent(request), () => {
          void navigate(requestPath(request.id));
        });
      }
    },
    [active, navigate],
  );
  useEffect(() => {
    data?.forEach(consider);
  }, [data, consider]);
  useNotifierStream(active, consider);

  return {
    support,
    on,
    active,
    turnOn: () => {
      void requestNotificationPermission().then((permission) => {
        setSupport(permission);
        if (permission === "granted") {
          setNotificationsPreference(true);
          setOn(true);
        }
      });
    },
    turnOff: () => {
      setNotificationsPreference(false);
      setOn(false);
    },
  };
}
