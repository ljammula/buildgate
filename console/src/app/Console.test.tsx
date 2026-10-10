import { act, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import type { HttpConfig } from "@/api/http";
import { Console } from "@/app/Console";
import { resolveHttpConfig } from "@/app/config";
import { type ConsoleSession, startSession } from "@/app/session";
import { getStoredGateToken, setStoredGateToken } from "@/platform/gateToken";

const gateSentence =
  "This console needs its gate token. On the host, run factoryd gate-token and paste the token it prints here.";
const expiredSentence = "The gate token this browser was using has expired or was replaced.";

interface Call {
  readonly url: string;
  readonly authorization: string | null;
}

/**
 * A server with the gate on: /console-config.json answers the bearer it was
 * sent, every other route answers "[]", or a 403 once `refuse` matches it.
 */
function gatedServer(accepted: readonly string[]) {
  const state = {
    accepted: new Set(accepted),
    refuse: (() => false) as (url: string) => boolean,
    calls: [] as Call[],
  };
  const fetch = ((input: RequestInfo | URL, init?: RequestInit) => {
    const url = input instanceof Request ? input.url : input.toString();
    const header = (init?.headers as Record<string, string> | undefined)?.Authorization ?? null;
    state.calls.push({ url, authorization: header });
    if (url === "/console-config.json") {
      const token = header?.replace(/^Bearer /, "") ?? "";
      const ok = state.accepted.has(token);
      const body = { writes_enabled: ok, gate: ok ? "accepted" : "required" };
      return Promise.resolve(new Response(JSON.stringify(body)));
    }
    if (state.refuse(url)) {
      return Promise.resolve(new Response('{"error": "forbidden"}', { status: 403 }));
    }
    return Promise.resolve(new Response("[]"));
  }) as typeof globalThis.fetch;
  const configCalls = () => state.calls.filter((call) => call.url === "/console-config.json");
  const otherCalls = () => state.calls.filter((call) => call.url !== "/console-config.json");
  return { state, fetch, configCalls, otherCalls };
}

function start(server: ReturnType<typeof gatedServer>, candidate: string | null = null) {
  const httpConfig = (gateToken: string | null): HttpConfig => ({
    ...resolveHttpConfig({
      baseUrl: "",
      auth: "",
      start: "",
      override: "",
      read: "",
      storedStartToken: null,
      storedGateToken: gateToken,
    }),
    fetch: server.fetch,
  });
  return startSession({ candidate, httpConfig });
}

const refused = (session: ConsoleSession, path: string) =>
  session.http.getJson(path, "read").catch((e: unknown) => e);

afterEach(() => {
  window.sessionStorage.clear();
  window.history.replaceState(null, "", "/");
});

test("a server that requires the gate gets the gate screen and no further call", async () => {
  const server = gatedServer(["good"]);
  render(<Console session={await start(server)} />);
  expect(screen.getByRole("heading", { name: "Gate token needed" })).toBeInTheDocument();
  expect(screen.getByText(/This console needs its gate token/).textContent).toBe(gateSentence);
  expect(screen.queryByText(expiredSentence)).not.toBeInTheDocument();
  expect(screen.queryByRole("navigation", { name: "Main" })).not.toBeInTheDocument();
  await new Promise((resolve) => setTimeout(resolve, 20));
  expect(server.state.calls).toEqual([{ url: "/console-config.json", authorization: null }]);
});

test("a stored token the server refuses is forgotten, and the screen says the link expired", async () => {
  setStoredGateToken("rotated-away");
  const server = gatedServer(["good"]);
  const session = await start(server);
  expect(session.storedTokenRefused).toBe(true);
  expect(getStoredGateToken()).toBeNull();
  render(<Console session={session} />);
  expect(screen.getByRole("heading", { name: "Gate token needed" })).toBeInTheDocument();
  expect(screen.getByText(expiredSentence)).toBeInTheDocument();
  expect(server.otherCalls()).toEqual([]);
});

test("a token from the link is stored once the server accepts it", async () => {
  const server = gatedServer(["good"]);
  const session = await start(server, "good");
  expect(getStoredGateToken()).toBe("good");
  expect(session.http.config.gateToken).toBe("good");
  expect(session.config).toMatchObject({ gate: "accepted", writesEnabled: true });
  expect(server.configCalls()).toEqual([
    { url: "/console-config.json", authorization: "Bearer good" },
  ]);
  render(<Console session={session} />);
  expect(screen.getByRole("navigation", { name: "Main" })).toBeInTheDocument();
  expect(screen.getByText("Read and write")).toBeInTheDocument();
});

test("a junk fragment does not replace a working stored token", async () => {
  setStoredGateToken("good");
  const server = gatedServer(["good"]);
  const session = await start(server, "junk");
  expect(getStoredGateToken()).toBe("good");
  expect(session.http.config.gateToken).toBe("good");
  expect(session.config.gate).toBe("accepted");
  expect(session.storedTokenRefused).toBe(false);
  expect(server.configCalls().map((call) => call.authorization)).toEqual([
    "Bearer junk",
    "Bearer good",
  ]);
  render(<Console session={session} />);
  expect(screen.getByRole("navigation", { name: "Main" })).toBeInTheDocument();
});

test("a refused link with nothing stored stores nothing and shows the gate screen", async () => {
  const server = gatedServer(["good"]);
  const session = await start(server, "junk");
  expect(window.sessionStorage.length).toBe(0);
  expect(session.http.config.gateToken).toBeNull();
  expect(session.storedTokenRefused).toBe(false);
  expect(server.configCalls()).toHaveLength(1);
  render(<Console session={session} />);
  expect(screen.getByRole("heading", { name: "Gate token needed" })).toBeInTheDocument();
  expect(screen.queryByText(expiredSentence)).not.toBeInTheDocument();
});

test("a link opened where no gate is needed is not stored", async () => {
  const fetch = (() =>
    Promise.resolve(new Response('{"writes_enabled":true}'))) as typeof globalThis.fetch;
  const session = await startSession({
    candidate: "unneeded",
    httpConfig: (gateToken) => ({
      baseUrl: "",
      readToken: null,
      startToken: null,
      overrideToken: null,
      gateToken,
      fetch,
    }),
  });
  expect(window.sessionStorage.length).toBe(0);
  expect(session.http.config.gateToken).toBeNull();
  expect(session.config.gate).toBe("off");
});

test("a 403 after the token was rotated clears it and shows the gate screen, with one re-check", async () => {
  setStoredGateToken("good");
  const server = gatedServer(["good"]);
  const session = await start(server);
  render(<Console session={session} />);
  expect(screen.getByRole("navigation", { name: "Main" })).toBeInTheDocument();

  server.state.accepted.clear();
  server.state.refuse = () => true;
  const before = server.configCalls().length;
  await act(async () => {
    await Promise.all([
      refused(session, "/runs"),
      refused(session, "/requests"),
      session.http
        .openStream("/requests/stream", "read", new AbortController().signal)
        .catch((e: unknown) => e),
    ]);
  });

  expect(await screen.findByRole("heading", { name: "Gate token needed" })).toBeInTheDocument();
  expect(screen.getByText(expiredSentence)).toBeInTheDocument();
  expect(screen.queryByRole("navigation", { name: "Main" })).not.toBeInTheDocument();
  expect(getStoredGateToken()).toBeNull();
  expect(session.gateLost()).toBe(true);
  expect(server.configCalls().length - before).toBe(1);

  // Once lost, a late 403 asks nothing more.
  await refused(session, "/runs");
  expect(server.configCalls().length - before).toBe(1);
});

test("a 403 while the server still accepts the token changes nothing", async () => {
  setStoredGateToken("good");
  const server = gatedServer(["good"]);
  const session = await start(server);
  render(<Console session={session} />);

  // What a start-token route answers a console that holds the gate token
  // alone. The gate token was not sent there, so nothing is asked.
  server.state.refuse = (url) => url === "/daemons" || url === "/runs/refused";
  const before = server.configCalls().length;
  await act(async () => {
    await session.http.getJson("/daemons", "start").catch((e: unknown) => e);
  });
  expect(server.configCalls().length).toBe(before);

  // A read the gate token was sent on and the server refused all the same.
  const daemons = () => session.http.getJson("/runs/refused", "read").catch((e: unknown) => e);
  await act(async () => {
    await Promise.all([daemons(), daemons()]);
  });
  await waitFor(() => {
    expect(server.configCalls().length - before).toBe(1);
  });
  expect(server.configCalls().at(-1)?.authorization).toBe("Bearer good");

  expect(session.gateLost()).toBe(false);
  expect(getStoredGateToken()).toBe("good");
  expect(screen.getByRole("navigation", { name: "Main" })).toBeInTheDocument();
  expect(screen.queryByRole("heading", { name: "Gate token needed" })).not.toBeInTheDocument();

  // The answer is not remembered: the next 403 asks again.
  await act(async () => {
    await daemons();
  });
  await waitFor(() => {
    expect(server.configCalls().length - before).toBe(2);
  });
  expect(session.gateLost()).toBe(false);
});

test("no call ever puts the gate token in a URL", async () => {
  setStoredGateToken("good");
  const server = gatedServer(["good", "fresh"]);
  const session = await start(server, "fresh");
  render(<Console session={session} />);
  await act(async () => {
    await session.http.sendJson("POST", "/requests", "gate", {});
  });
  expect(server.state.calls.length).toBeGreaterThan(1);
  for (const call of server.state.calls) expect(call.url).not.toMatch(/good|fresh/);
});

test("a pasted token the server accepts is stored for the tab and the console starts again", async () => {
  const server = gatedServer(["good"]);
  const restart = vi.fn();
  render(<Console session={await start(server)} restart={restart} />);
  const field = screen.getByLabelText("Gate token");
  // Not a password field: a browser offers to save one, and a saved token
  // would outlive the tab.
  expect(field).toHaveAttribute("type", "text");
  expect(field).toHaveAttribute("autocomplete", "off");
  const open = screen.getByRole("button", { name: "Open console" });
  expect(open).toBeDisabled();

  await userEvent.type(field, "  good  {Enter}");
  await waitFor(() => {
    expect(restart).toHaveBeenCalledTimes(1);
  });
  expect(getStoredGateToken()).toBe("good");
  expect(window.location.href).not.toContain("good");
  expect(server.configCalls().at(-1)).toEqual({
    url: "/console-config.json",
    authorization: "Bearer good",
  });
  expect(server.otherCalls()).toEqual([]);

  // Starting again finds the stored token and opens the app.
  const session = await start(server);
  expect(session.config.gate).toBe("accepted");
  expect(session.http.config.gateToken).toBe("good");
});

test("a pasted token the server refuses is not stored, and the screen says so", async () => {
  const server = gatedServer(["good"]);
  const restart = vi.fn();
  render(<Console session={await start(server)} restart={restart} />);
  await userEvent.type(screen.getByLabelText("Gate token"), "stale");
  await userEvent.click(screen.getByRole("button", { name: "Open console" }));
  expect(await screen.findByRole("alert")).toHaveTextContent("That token was not accepted");
  expect(server.configCalls().at(-1)?.authorization).toBe("Bearer stale");
  expect(restart).not.toHaveBeenCalled();
  expect(getStoredGateToken()).toBeNull();
  expect(screen.getByLabelText("Gate token")).toHaveValue("");
  expect(server.otherCalls()).toEqual([]);
});

test("a server that does not answer leaves the pasted token in the field and does not call it refused", async () => {
  const server = gatedServer(["good"]);
  const restart = vi.fn();
  render(<Console session={await start(server)} restart={restart} />);
  // From here the config route fails, as it does behind a proxy that is down.
  const answering = server.fetch;
  server.fetch = () => Promise.reject(new TypeError("network"));
  await userEvent.type(screen.getByLabelText("Gate token"), "good{Enter}");
  expect(await screen.findByRole("alert")).toHaveTextContent("The server did not answer");
  expect(screen.getByLabelText("Gate token")).toHaveValue("good");
  expect(getStoredGateToken()).toBeNull();
  expect(restart).not.toHaveBeenCalled();
  // The same token, once the server answers again.
  server.fetch = answering;
  await userEvent.click(screen.getByRole("button", { name: "Open console" }));
  await waitFor(() => {
    expect(restart).toHaveBeenCalledTimes(1);
  });
  expect(getStoredGateToken()).toBe("good");
});

test("after the gate is lost mid-session, the new token can be pasted", async () => {
  setStoredGateToken("good");
  const server = gatedServer(["good"]);
  const session = await start(server);
  const restart = vi.fn();
  render(<Console session={session} restart={restart} />);
  server.state.accepted = new Set(["rotated"]);
  await act(async () => {
    await refused(session, "/runs");
  });
  server.state.refuse = (url) => url === "/runs";
  await act(async () => {
    await refused(session, "/runs");
  });
  await screen.findByRole("heading", { name: "Gate token needed" });
  await userEvent.type(screen.getByLabelText("Gate token"), "rotated{Enter}");
  await waitFor(() => {
    expect(restart).toHaveBeenCalledTimes(1);
  });
  expect(getStoredGateToken()).toBe("rotated");
});
