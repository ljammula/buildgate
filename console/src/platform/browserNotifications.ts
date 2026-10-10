// The browser's notification and Web Locks APIs, and the operator's choice to
// use them. Every call is guarded: a browser without the API, or one that
// throws (private mode, an insecure origin), reads as "unsupported". A
// notification carries text and a click only: never a token, never an action.

export type NotificationSupport = "unsupported" | "default" | "granted" | "denied";

export interface NotificationContent {
  readonly title: string;
  readonly body: string;
  readonly tag: string;
}

const preferenceKey = "factoryBrowserNotifications";
const lockName = "buildgate-notifier";

export function notificationSupport(): NotificationSupport {
  try {
    if (typeof window === "undefined" || !("Notification" in window)) return "unsupported";
    const permission = window.Notification.permission;
    return permission === "granted" || permission === "denied" ? permission : "default";
  } catch {
    return "unsupported";
  }
}

/** Asks the browser for permission. Browsers only show the prompt from a click handler. */
export async function requestNotificationPermission(): Promise<NotificationSupport> {
  try {
    if (notificationSupport() === "unsupported") return "unsupported";
    await window.Notification.requestPermission();
  } catch {
    // A refused or failed prompt leaves the permission as the browser has it.
  }
  return notificationSupport();
}

/**
 * Raises one notification. Its click focuses this tab, runs `onClick`, and
 * closes it. A tag replaces an earlier notification that has the same one.
 */
export function showNotification(content: NotificationContent, onClick: () => void): void {
  try {
    if (notificationSupport() !== "granted") return;
    const notification = new window.Notification(content.title, {
      body: content.body,
      tag: content.tag,
    });
    notification.onclick = () => {
      window.focus();
      onClick();
      notification.close();
    };
  } catch {
    // Some browsers refuse the constructor outside a service worker.
  }
}

/**
 * Takes the console-wide notifier lock and calls `onAcquired` once this tab
 * holds it: one tab per browser raises notifications. The lock is held until
 * the returned function is called or the tab closes. Without Web Locks every
 * tab is its own notifier, and the notification tag de-duplicates at the OS.
 */
export function holdNotifierLock(onAcquired: () => void): () => void {
  let release: (() => void) | null = null;
  let released = false;
  const locks = locksApi();
  if (locks === null) {
    onAcquired();
    return () => undefined;
  }
  const request = locks.request(lockName, { mode: "exclusive" }, () => {
    if (released) return undefined;
    onAcquired();
    return new Promise<void>((resolve) => {
      release = resolve;
    });
  });
  // A rejected request (a lock manager that refuses) leaves this tab quiet.
  request.catch(() => undefined);
  return () => {
    released = true;
    release?.();
  };
}

function locksApi(): LockManager | null {
  try {
    return typeof navigator !== "undefined" && "locks" in navigator ? navigator.locks : null;
  } catch {
    return null;
  }
}

function storage(): Storage | null {
  try {
    return window.localStorage;
  } catch {
    return null;
  }
}

/** Whether the operator turned browser notifications on in this browser. */
export function getNotificationsPreference(): boolean {
  try {
    return storage()?.getItem(preferenceKey) === "on";
  } catch {
    return false;
  }
}

export function setNotificationsPreference(on: boolean): void {
  try {
    if (on) storage()?.setItem(preferenceKey, "on");
    else storage()?.removeItem(preferenceKey);
  } catch {
    // Storage may throw in private browsing mode.
  }
}

const watchingKey = "factoryNotifierWatching";
const watchingGapMs = 2 * 60 * 1000;

/**
 * The time (ms) from which this tab has been looking at the requests: now,
 * or, after a reload, when it last looked before it, so what was notified
 * while the page loaded is still news. A gap longer than two minutes is a
 * new look.
 */
export function watchingSince(now: number): number {
  try {
    const stored = Number(window.sessionStorage.getItem(watchingKey));
    if (Number.isFinite(stored) && stored > 0 && stored <= now && now - stored < watchingGapMs) {
      return stored;
    }
  } catch {
    // No storage: this look starts now.
  }
  return now;
}

/** Records that this tab looked at the requests at `now`. */
export function recordWatching(now: number): void {
  try {
    window.sessionStorage.setItem(watchingKey, String(now));
  } catch {
    // Storage may throw in private browsing mode.
  }
}
