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
};

test("each route's token goes in the Authorization header, never the URL", async () => {
  const { fetch, calls } = recording(() => new Response("[]"));
  const http = createHttp({ baseUrl: "", ...tokens, fetch });
  await http.getJson("/runs", "read");
  await http.getJson("/daemons", "start");
  await http.sendJson("POST", "/requests/req-1/approve", "override", { by: "alice" });
  expect(calls.map((c) => c.init.headers)).toEqual([
    { Authorization: "Bearer read-secret" },
    { Authorization: "Bearer start-secret" },
    { "Content-Type": "application/json", Authorization: "Bearer override-secret" },
  ]);
  for (const call of calls) expect(call.url).not.toMatch(/secret/);
});

test("a write carries Content-Type: application/json even with no token", async () => {
  const { fetch, calls } = recording(() => new Response("{}"));
  const http = createHttp({
    baseUrl: "",
    readToken: null,
    startToken: null,
    overrideToken: null,
    fetch,
  });
  await http.sendJson("PUT", "/requests/req-1/spec", "override", { content: "# Spec\n" });
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
