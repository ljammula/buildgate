import {
  getNotificationsPreference,
  holdNotifierLock,
  notificationSupport,
  recordWatching,
  requestNotificationPermission,
  setNotificationsPreference,
  showNotification,
  watchingSince,
} from "@/platform/browserNotifications";

interface FakeNotification {
  title: string;
  options: { body: string; tag: string };
  onclick: (() => void) | null;
  close: ReturnType<typeof vi.fn>;
}

function stubNotification(permission: string, onRequest?: () => void) {
  const made: FakeNotification[] = [];
  class Fake {
    static permission = permission;
    static requestPermission = vi.fn(() => {
      onRequest?.();
      return Promise.resolve(Fake.permission);
    });
    onclick: (() => void) | null = null;
    close = vi.fn();
    constructor(
      public title: string,
      public options: { body: string; tag: string },
    ) {
      made.push(this);
    }
  }
  vi.stubGlobal("Notification", Fake);
  return { Fake, made };
}

function stubLocks(locks: unknown) {
  Object.defineProperty(navigator, "locks", { value: locks, configurable: true });
}

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  Reflect.deleteProperty(navigator, "locks");
  window.localStorage.clear();
});

describe("notificationSupport", () => {
  test("is unsupported without the API", () => {
    Reflect.deleteProperty(window, "Notification");
    expect(notificationSupport()).toBe("unsupported");
  });

  test.each(["default", "granted", "denied"] as const)("reports %s", (permission) => {
    stubNotification(permission);
    expect(notificationSupport()).toBe(permission);
  });

  test("is unsupported when reading the permission throws", () => {
    const throwing = {};
    Object.defineProperty(throwing, "permission", {
      get: () => {
        throw new Error("blocked");
      },
    });
    vi.stubGlobal("Notification", throwing);
    expect(notificationSupport()).toBe("unsupported");
  });
});

describe("requestNotificationPermission", () => {
  test("asks the browser and returns the answer", async () => {
    const { Fake } = stubNotification("default", () => {
      Fake.permission = "granted";
    });
    await expect(requestNotificationPermission()).resolves.toBe("granted");
    expect(Fake.requestPermission).toHaveBeenCalledTimes(1);
  });

  test("never prompts where there is no API", async () => {
    Reflect.deleteProperty(window, "Notification");
    await expect(requestNotificationPermission()).resolves.toBe("unsupported");
  });
});

describe("showNotification", () => {
  const content = { title: "Spec ready", body: "app: add", tag: "req-1:spec_review" };

  test("raises nothing without permission", () => {
    const { made } = stubNotification("default");
    showNotification(content, () => undefined);
    expect(made).toHaveLength(0);
  });

  test("raises title, body and tag; a click focuses, runs the callback, then closes", () => {
    const { made } = stubNotification("granted");
    const order: string[] = [];
    vi.spyOn(window, "focus").mockImplementation(() => {
      order.push("focus");
    });
    showNotification(content, () => {
      order.push("open");
    });
    expect(made).toHaveLength(1);
    expect(made[0]!.title).toBe("Spec ready");
    expect(made[0]!.options).toEqual({ body: "app: add", tag: "req-1:spec_review" });
    made[0]!.close.mockImplementation(() => {
      order.push("close");
    });
    made[0]!.onclick?.();
    expect(order).toEqual(["focus", "open", "close"]);
  });

  test("a constructor that throws is swallowed", () => {
    const refusing = function Refusing() {
      throw new TypeError("Illegal constructor");
    };
    refusing.permission = "granted";
    vi.stubGlobal("Notification", refusing);
    expect(() => {
      showNotification(content, () => undefined);
    }).not.toThrow();
  });
});

describe("holdNotifierLock", () => {
  test("without Web Locks the tab is the notifier at once", () => {
    const acquired = vi.fn();
    holdNotifierLock(acquired)();
    expect(acquired).toHaveBeenCalledTimes(1);
  });

  test("requests the exclusive buildgate-notifier lock and holds it until released", async () => {
    let finished = false;
    const request = vi.fn(
      async (_name: string, _options: unknown, callback: () => Promise<void> | undefined) => {
        await callback();
        finished = true;
      },
    );
    stubLocks({ request });
    const acquired = vi.fn();
    const release = holdNotifierLock(acquired);
    await Promise.resolve();
    expect(request.mock.calls[0]![0]).toBe("buildgate-notifier");
    expect(request.mock.calls[0]![1]).toEqual({ mode: "exclusive" });
    expect(acquired).toHaveBeenCalledTimes(1);
    expect(finished).toBe(false);
    release();
    await Promise.resolve();
    await Promise.resolve();
    expect(finished).toBe(true);
  });

  test("a release before the lock is granted means the tab never becomes the notifier", async () => {
    let grant: (() => Promise<void> | undefined) | null = null;
    stubLocks({
      request: (_n: string, _o: unknown, callback: () => Promise<void> | undefined) => {
        grant = callback;
        return Promise.resolve();
      },
    });
    const acquired = vi.fn();
    holdNotifierLock(acquired)();
    await grant!();
    expect(acquired).not.toHaveBeenCalled();
  });

  test("a lock manager that rejects leaves the tab quiet and unthrowing", async () => {
    stubLocks({ request: () => Promise.reject(new Error("refused")) });
    const acquired = vi.fn();
    expect(() => holdNotifierLock(acquired)).not.toThrow();
    await Promise.resolve();
    expect(acquired).not.toHaveBeenCalled();
  });
});

describe("the preference", () => {
  test("is off until turned on, and stored as on", () => {
    expect(getNotificationsPreference()).toBe(false);
    setNotificationsPreference(true);
    expect(window.localStorage.getItem("factoryBrowserNotifications")).toBe("on");
    expect(getNotificationsPreference()).toBe(true);
    setNotificationsPreference(false);
    expect(getNotificationsPreference()).toBe(false);
  });

  test("a stored value other than on reads as off", () => {
    window.localStorage.setItem("factoryBrowserNotifications", "yes");
    expect(getNotificationsPreference()).toBe(false);
  });

  test("storage that throws reads as off and writes nothing", () => {
    vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => {
      throw new Error("denied");
    });
    vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => {
      throw new Error("denied");
    });
    expect(getNotificationsPreference()).toBe(false);
    expect(() => {
      setNotificationsPreference(true);
    }).not.toThrow();
  });
});

describe("watchingSince", () => {
  afterEach(() => {
    window.sessionStorage.clear();
  });

  test("is now for a tab that has not looked before", () => {
    expect(watchingSince(5_000_000)).toBe(5_000_000);
  });

  test("is the last look across a reload, and now again after a long gap", () => {
    recordWatching(5_000_000);
    expect(watchingSince(5_030_000)).toBe(5_000_000);
    expect(watchingSince(5_000_000 + 3 * 60 * 1000)).toBe(5_000_000 + 3 * 60 * 1000);
  });

  test("ignores a stored time that is not a past time", () => {
    window.sessionStorage.setItem("factoryNotifierWatching", "soon");
    expect(watchingSince(5_000_000)).toBe(5_000_000);
    recordWatching(9_000_000);
    expect(watchingSince(5_000_000)).toBe(5_000_000);
  });
});
