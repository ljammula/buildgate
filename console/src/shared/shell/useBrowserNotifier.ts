import { useEffect, useRef, useState } from "react";
import { useNavigate } from "react-router";

import { needsYouPollMs } from "@/api/polling";
import { useRequests } from "@/api/requestQueries";
import { type SeenNotifications, attend, notificationContent } from "@/domain/attention";
import {
  type NotificationSupport,
  getNotificationsPreference,
  holdNotifierLock,
  notificationSupport,
  requestNotificationPermission,
  setNotificationsPreference,
  showNotification,
} from "@/platform/browserNotifications";
import { requestPath } from "@/routes/paths";

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
 * operator, from the request list the shell already keeps current. Of several
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
  useEffect(() => {
    if (data === undefined) return;
    for (const request of data) {
      const attention = attend(seen.current, request);
      seen.current = attention.seen;
      if (attention.raise && active) {
        showNotification(notificationContent(request), () => {
          void navigate(requestPath(request.id));
        });
      }
    }
  }, [data, active, navigate]);

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
