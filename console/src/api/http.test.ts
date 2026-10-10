import { createHttp, isAbort } from "@/api/http";
import { ApiError } from "@/domain/apiError";

function recording(response: () => Response) {
  const calls: { url: string; init: RequestInit }[] = [];
  const fetch = ((input: RequestInfo | URL, init?: RequestInit) => {
    calls.push({ url: input instanceof Request ? input.url : input.toString(), init: init ?? {} });
    return Promise.resolve(response());
  }) as typeof globalThis.fetch;
  return { fetch, calls };
}

const tokens = {
  readToken: "read-secret",
  startToken: "start-secret",
  overrideToken: "override-secret",
  gateToken: "gate-secret",
};

const noTokens = { readToken: null, startToken: null, overrideToken: null, gateToken: null };
const bearer = (token: string) => ({ Authorization: `Bearer ${token}` });

/** The Authorization header each token kind sends on a GET under `config`. */
async function sentBy(config: Partial<typeof tokens>) {
  const { fetch, calls } = recording(() => new Response("[]"));
  const http = createHttp({ baseUrl: "", ...noTokens, ...config, fetch });
  await http.getJson("/a", "read");
  await http.getJson("/a", "start");
  await http.getJson("/a", "override");
  await http.getJson("/a", "gate");
  for (const call of calls) expect(call.url).toBe("/a");
  const [read, start, override, gate] = calls.map((c) => c.init.headers);
  return { read, start, override, gate };
}

test("each route's token goes in the Authorization header, never the URL", async () => {
  const { fetch, calls } = recording(() => new Response("[]"));
  const http = createHttp({ baseUrl: "", ...tokens, fetch });
  await http.getJson("/runs", "read");
  await http.getJson("/daemons", "start");
  await http.sendJson("POST", "/runs/run-1/override", "override", { by: "alice" });
  await http.sendJson("POST", "/requests/req-1/approve", "gate", { by: "alice" });
  await http.getBytes("/requests/req-1/oracle/x", "read");
  await http.openStream("/requests/stream", "read", new AbortController().signal);
  expect(calls.map((c) => c.init.headers)).toEqual([
    { Authorization: "Bearer read-secret" },
    { Authorization: "Bearer start-secret" },
    { "Content-Type": "application/json", Authorization: "Bearer override-secret" },
    { "Content-Type": "application/json", Authorization: "Bearer gate-secret" },
    { Authorization: "Bearer read-secret" },
    { Authorization: "Bearer read-secret" },
  ]);
  for (const call of calls) {
    expect(call.url).not.toMatch(/secret/);
    expect(call.url).not.toContain("?");
  }
});

test("with every token configured each kind sends its own", async () => {
  expect(await sentBy(tokens)).toEqual({
    read: bearer("read-secret"),
    start: bearer("start-secret"),
    override: bearer("override-secret"),
    gate: bearer("gate-secret"),
  });
});

test("the gate token alone goes to reads and request writes, never to start or override routes", async () => {
  expect(await sentBy({ gateToken: "gate-secret" })).toEqual({
    read: bearer("gate-secret"),
    start: {},
    override: {},
    gate: bearer("gate-secret"),
  });
});

test("a request write falls back to the override token of a bundle built with one", async () => {
  expect(await sentBy({ overrideToken: "override-secret" })).toEqual({
    read: {},
    start: {},
    override: bearer("override-secret"),
    gate: bearer("override-secret"),
  });
});

test("the override kind never carries the gate token, whatever else is set", async () => {
  for (const config of [
    { gateToken: "gate-secret" },
    { gateToken: "gate-secret", readToken: "read-secret", startToken: "start-secret" },
    { gateToken: "gate-secret", overrideToken: "" },
  ]) {
    expect((await sentBy(config)).override).toEqual({});
  }
  expect(
    (await sentBy({ gateToken: "gate-secret", overrideToken: "override-secret" })).override,
  ).toEqual(bearer("override-secret"));
});

test("the read token wins over the gate token on a read; an empty one does not", async () => {
  expect((await sentBy({ readToken: "read-secret", gateToken: "gate-secret" })).read).toEqual(
    bearer("read-secret"),
  );
  expect((await sentBy({ readToken: "", gateToken: "gate-secret" })).read).toEqual(
    bearer("gate-secret"),
  );
});

test("with no token configured no kind sends a header", async () => {
  expect(await sentBy({})).toEqual({ read: {}, start: {}, override: {}, gate: {} });
});

test("every method reports a 403 to the hook for a call the gate token was sent on", async () => {
  const { fetch } = recording(() => new Response('{"error": "forbidden"}', { status: 403 }));
  const onForbidden = vi.fn();
  const http = createHttp(
    { baseUrl: "", ...noTokens, gateToken: "gate-secret", fetch },
    {
      onForbidden,
    },
  );
  const failures = await Promise.all(
    [
      http.getJson("/runs", "read"),
      http.getBytes("/runs/run-1/log", "read"),
      http.sendJson("POST", "/requests", "gate", {}),
      http.openStream("/requests/stream", "read", new AbortController().signal),
      // Routes the gate token is not sent to say nothing about it: not reported.
      http.getJson("/daemons", "start"),
      http.sendJson("POST", "/runs/run-1/override", "override", {}),
    ].map((call) => call.catch((e: unknown) => e)),
  );
  expect(onForbidden).toHaveBeenCalledTimes(4);
  for (const failure of failures) {
    expect(failure).toBeInstanceOf(ApiError);
    expect((failure as ApiError).status).toBe(403);
    expect((failure as ApiError).message).not.toMatch(/secret/);
  }
});

test("the 403 hook stays quiet for other statuses and when no gate token is configured", async () => {
  const onForbidden = vi.fn();
  for (const status of [401, 404, 500]) {
    const { fetch } = recording(() => new Response("{}", { status }));
    const http = createHttp({ baseUrl: "", ...tokens, fetch }, { onForbidden });
    await http.getJson("/runs", "read").catch(() => undefined);
  }
  const { fetch } = recording(() => new Response("{}", { status: 403 }));
  const http = createHttp({ baseUrl: "", ...tokens, gateToken: null, fetch }, { onForbidden });
  await http.getJson("/runs", "read").catch(() => undefined);
  expect(onForbidden).not.toHaveBeenCalled();
});

test("a write carries Content-Type: application/json even with no token", async () => {
  const { fetch, calls } = recording(() => new Response("{}"));
  const http = createHttp({
    baseUrl: "",
    readToken: null,
    startToken: null,
    overrideToken: null,
    gateToken: null,
    fetch,
  });
  await http.sendJson("PUT", "/requests/req-1/spec", "gate", { content: "# Spec\n" });
  expect(calls[0]!.init.headers).toEqual({ "Content-Type": "application/json" });
  expect(calls[0]!.init.method).toBe("PUT");
  expect(calls[0]!.init.body).toBe('{"content":"# Spec\\n"}');
});

test("no request asks for text/html, which would return the console shell", async () => {
  const { fetch, calls } = recording(() => new Response("{}"));
  const http = createHttp({ baseUrl: "", ...tokens, fetch });
  await http.getJson("/requests/req-1", "read");
  await http.getBytes("/requests/req-1/oracle/RUN_COMMAND.txt", "read");
  for (const call of calls) expect(JSON.stringify(call.init.headers)).not.toMatch(/accept/i);
});

test("an empty base URL keeps the path relative; a base URL resolves it", () => {
  const same = createHttp({ baseUrl: "", ...tokens });
  expect(same.url("/runs/run-1")).toBe("/runs/run-1");
  const other = createHttp({ baseUrl: "http://127.0.0.1:8090", ...tokens });
  expect(other.url("/runs/run-1")).toBe("http://127.0.0.1:8090/runs/run-1");
});

test("a non-2xx throws ApiError with the status and the server's message", async () => {
  const { fetch } = recording(() => new Response('{"error": "run not found"}', { status: 404 }));
  const http = createHttp({ baseUrl: "", ...tokens, fetch });
  const error = await http.getJson("/runs/missing", "read").catch((e: unknown) => e);
  expect(error).toBeInstanceOf(ApiError);
  expect((error as ApiError).status).toBe(404);
  expect((error as ApiError).serverMessage).toBe("run not found");
});

test("the body is decoded as UTF-8 whatever charset the response declares", async () => {
  const bytes = new TextEncoder().encode('{"title": "Résumé — ✓"}');
  const { fetch } = recording(
    () =>
      new Response(bytes, { headers: { "Content-Type": "application/json; charset=iso-8859-1" } }),
  );
  const http = createHttp({ baseUrl: "", ...tokens, fetch });
  expect(await http.getJson("/requests/req-1", "read")).toEqual({ title: "Résumé — ✓" });
});

test("getBytes returns the bytes as received, undecoded", async () => {
  const { fetch } = recording(() => new Response(new Uint8Array([0xff, 0xfe, 0x00])));
  const http = createHttp({ baseUrl: "", ...tokens, fetch });
  expect(Array.from(await http.getBytes("/requests/r/oracle/x", "read"))).toEqual([
    0xff, 0xfe, 0x00,
  ]);
});

test("a write with an empty response body resolves to null", async () => {
  const { fetch } = recording(() => new Response(null, { status: 204 }));
  const http = createHttp({ baseUrl: "", ...tokens, fetch });
  expect(await http.sendJson("PUT", "/x", "override", {})).toBeNull();
});

test("isAbort recognises a cancelled request", () => {
  expect(isAbort(new DOMException("aborted", "AbortError"))).toBe(true);
  expect(isAbort(new TypeError("network down"))).toBe(false);
});
