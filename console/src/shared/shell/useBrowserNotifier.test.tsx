import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { useRequestBoard } from "@/api/requestQueries";
import {
  getNotificationsPreference,
  setNotificationsPreference,
} from "@/platform/browserNotifications";
import { AppShell } from "@/shared/shell/AppShell";
import { requestJson } from "@/test/requestFixtures";
import { type FakeRoute, json, renderApp, sseResponse } from "@/test/render";

// `first` was sent before any test's tab started looking; `second` after.
const first = "2026-10-10T09:00:00Z";
const second = new Date(Date.now() + 60 * 60 * 1000).toISOString();

interface MadeNotification {
  title: string;
  options: { body: string; tag: string };
  onclick: (() => void) | null;
  close: ReturnType<typeof vi.fn>;
}

function stubNotifications(permission: string): MadeNotification[] {
  const made: MadeNotification[] = [];
  class Fake {
    static permission = permission;
    static requestPermission = vi.fn(() => Promise.resolve(Fake.permission));
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
  return made;
}

function list(notifiedAt: string): FakeRoute[] {
  return [
    {
      on: "GET /requests",
      reply: () =>
        json([
          requestJson({
            id: "req-1",
            state: "spec_review",
            title: "Add a coupon field",
            project: "checkouts",
            lastNotifiedAt: notifiedAt,
            lastAsk: "Spec ready for your review",
          }),
        ]),
    },
    { on: "GET /requests/events", reply: sseResponse("state") },
    { on: "GET /requests/events?notifier=1", reply: sseResponse("state") },
  ];
}

function Board() {
  useRequestBoard();
  return <p>board</p>;
}

function renderShell(routes: FakeRoute[]) {
  return renderApp(
    <AppShell>
      <Board />
    </AppShell>,
    { server: routes },
  );
}

afterEach(() => {
  vi.unstubAllGlobals();
  setNotificationsPreference(false);
});

test("a tab that is the notifier opens the stream with notifier=1, and says nothing for what it found", async () => {
  const made = stubNotifications("granted");
  setNotificationsPreference(true);
  const { server } = renderShell(list(first));
  await waitFor(() => {
    expect(server.sent("GET /requests/events?notifier=1")).toHaveLength(1);
  });
  // The board's own stream is the plain one: only the shell's says notifier.
  expect(server.sent("GET /requests/events")).toHaveLength(1);
  expect(screen.getByRole("button", { name: "Notifications on" })).toBeInTheDocument();
  expect(made).toHaveLength(0);
});

test("a later notification raises one, and a click opens that request's page", async () => {
  const made = stubNotifications("granted");
  setNotificationsPreference(true);
  const focus = vi.spyOn(window, "focus").mockImplementation(() => undefined);
  const { server, queryClient, location } = renderShell(list(first));
  await waitFor(() => {
    expect(server.sent("GET /requests")).not.toHaveLength(0);
    expect(screen.getByRole("button", { name: "Notifications on" })).toBeInTheDocument();
  });
  await waitFor(() => {
    expect(server.sent("GET /requests/events?notifier=1")).toHaveLength(1);
  });

  server.set("GET /requests", () =>
    json([
      requestJson({
        id: "req-1",
        state: "spec_review",
        title: "Add a coupon field",
        project: "checkouts",
        lastNotifiedAt: second,
        lastAsk: "Spec ready for your review",
      }),
    ]),
  );
  await queryClient.invalidateQueries({ queryKey: ["requests"] });

  await waitFor(() => {
    expect(made).toHaveLength(1);
  });
  expect(made[0]!.title).toBe("Spec ready for your review");
  expect(made[0]!.options).toEqual({
    body: "checkouts: Add a coupon field",
    tag: `req-1:${second}`,
  });
  made[0]!.onclick?.();
  expect(focus).toHaveBeenCalled();
  expect(made[0]!.close).toHaveBeenCalled();
  await waitFor(() => {
    expect(location()).toBe("/requests/req-1");
  });
});

test("with the preference off, the stream has no notifier parameter and nothing is raised", async () => {
  const made = stubNotifications("granted");
  const { server } = renderShell(list(first));
  await waitFor(() => {
    expect(server.sent("GET /requests/events")).toHaveLength(1);
  });
  expect(server.sent("GET /requests/events?notifier=1")).toHaveLength(0);
  expect(screen.getByRole("button", { name: "Turn on notifications" })).toBeInTheDocument();
  expect(made).toHaveLength(0);
});

test("turning notifications on asks for permission, stores the choice and opens the notifier stream; turning them off closes it", async () => {
  stubNotifications("default");
  const Fake = window.Notification as unknown as {
    permission: string;
    requestPermission: () => Promise<string>;
  };
  Fake.requestPermission = () => {
    Fake.permission = "granted";
    return Promise.resolve("granted");
  };
  const { server } = renderShell(list(first));
  await userEvent.click(await screen.findByRole("button", { name: "Turn on notifications" }));
  expect(await screen.findByRole("button", { name: "Notifications on" })).toBeInTheDocument();
  expect(getNotificationsPreference()).toBe(true);
  await waitFor(() => {
    expect(server.sent("GET /requests/events?notifier=1")).toHaveLength(1);
  });

  await userEvent.click(screen.getByRole("button", { name: "Notifications on" }));
  expect(await screen.findByRole("button", { name: "Turn on notifications" })).toBeInTheDocument();
  expect(getNotificationsPreference()).toBe(false);
  // Off again: no second notifier stream is opened, and the board's is untouched.
  expect(server.sent("GET /requests/events?notifier=1")).toHaveLength(1);
  expect(server.sent("GET /requests/events")).toHaveLength(1);
});

test("the notifier stream is held on a screen with no stream of its own, and its frames raise", async () => {
  const made = stubNotifications("granted");
  setNotificationsPreference(true);
  const routes = list(first).filter((route) => route.on !== "GET /requests/events?notifier=1");
  const frame = (lastNotifiedAt: string) =>
    requestJson({
      id: "req-1",
      state: "spec_review",
      title: "Add a coupon field",
      project: "checkouts",
      lastNotifiedAt,
      lastAsk: "Spec ready for your review",
    });
  const { server } = renderApp(
    <AppShell>
      <p>ops</p>
    </AppShell>,
    {
      server: [
        ...routes,
        {
          on: "GET /requests/events?notifier=1",
          reply: sseResponse("state", [frame(first), frame(second)]),
        },
      ],
    },
  );
  await waitFor(() => {
    expect(made).toHaveLength(1);
  });
  expect(server.sent("GET /requests/events")).toHaveLength(0);
});

test("a blocked browser shows the note and never the notifier parameter", async () => {
  stubNotifications("denied");
  setNotificationsPreference(true);
  const { server } = renderShell(list(first));
  expect(screen.getByText("Notifications blocked in this browser")).toBeInTheDocument();
  await waitFor(() => {
    expect(server.sent("GET /requests/events")).toHaveLength(1);
  });
  expect(server.sent("GET /requests/events?notifier=1")).toHaveLength(0);
});

test("a browser without notifications shows no control", () => {
  Reflect.deleteProperty(window, "Notification");
  renderShell(list(first));
  expect(screen.queryByText(/notifications/i)).not.toBeInTheDocument();
});
