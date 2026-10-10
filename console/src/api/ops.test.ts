import { createHttp } from "@/api/http";
import { fetchConsoleConfig, getQueueRunStatus, listDaemons, listWorkspaces } from "@/api/ops";
import { readFixtureText } from "@/test/fixtures";

interface Call {
  readonly url: string;
  readonly init: RequestInit;
}

function recordingFetch(respond: (call: Call) => Response | Promise<Response>) {
  const calls: Call[] = [];
  const fetch = vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
    const call = { url: input as string, init: init ?? {} };
    calls.push(call);
    return Promise.resolve(respond(call));
  });
  return { fetch: fetch as unknown as typeof globalThis.fetch, calls };
}

const httpFor = (fetch: typeof globalThis.fetch, baseUrl = "") =>
  createHttp({
    baseUrl,
    readToken: "read-t",
    startToken: "start-t",
    overrideToken: "override-t",
    gateToken: "gate-t",
    fetch,
  });

const json = (body: string, status = 200) =>
  new Response(body, { status, headers: { "content-type": "application/json" } });

const disabled = {
  writesEnabled: false,
  gate: "off",
  temporalUiUrl: null,
  releasePolicyWarning: null,
};

describe("fetchConsoleConfig", () => {
  test("decodes the fixture and sends the gate token alone", async () => {
    const { fetch, calls } = recordingFetch(() => json(readFixtureText("api/console-config.json")));
    const config = await fetchConsoleConfig(httpFor(fetch));
    expect(calls).toHaveLength(1);
    expect(calls[0]?.url).toBe("/console-config.json");
    expect(calls[0]?.init.method).toBe("GET");
    expect(calls[0]?.init.headers).toEqual({ Authorization: "Bearer gate-t" });
    expect(config.writesEnabled).toBe(false);
    expect(config.temporalUiUrl).toBe("http://localhost:8233");
    expect(config.releasePolicyWarning).toContain("release policy denies every PR");
  });

  test("resolves against a base URL", async () => {
    const { fetch, calls } = recordingFetch(() => json('{"writes_enabled":true}'));
    const config = await fetchConsoleConfig(httpFor(fetch, "http://factory.test"));
    expect(calls[0]?.url).toBe("http://factory.test/console-config.json");
    expect(config).toEqual({
      writesEnabled: true,
      gate: "off",
      temporalUiUrl: null,
      releasePolicyWarning: null,
    });
  });

  test("with no gate token it sends no credential, whatever else is configured", async () => {
    const { fetch, calls } = recordingFetch(() => json('{"gate":"required"}'));
    const http = createHttp({
      baseUrl: "",
      readToken: "read-t",
      startToken: "start-t",
      overrideToken: "override-t",
      gateToken: null,
      fetch,
    });
    const config = await fetchConsoleConfig(http);
    expect(calls[0]?.init.headers).toEqual({});
    expect(config.gate).toBe("required");
  });

  test("decodes the server's answer to the gate token", async () => {
    const { fetch } = recordingFetch(() => json('{"writes_enabled":true,"gate":"accepted"}'));
    const config = await fetchConsoleConfig(httpFor(fetch));
    expect(config.gate).toBe("accepted");
    expect(config.writesEnabled).toBe(true);
  });

  test("a 403 here does not call the 403 hook of the Http it was given", async () => {
    const { fetch } = recordingFetch(() => json('{"error":"forbidden"}', 403));
    const onForbidden = vi.fn();
    const http = createHttp(
      {
        baseUrl: "",
        readToken: null,
        startToken: null,
        overrideToken: null,
        gateToken: "gate-t",
        fetch,
      },
      { onForbidden },
    );
    expect(await fetchConsoleConfig(http)).toEqual(disabled);
    expect(onForbidden).not.toHaveBeenCalled();
  });

  test("a 404 from a server predating the route reads as writes not enabled", async () => {
    const { fetch } = recordingFetch(() => json('{"error":"not found"}', 404));
    expect(await fetchConsoleConfig(httpFor(fetch))).toEqual(disabled);
  });

  test("a 500 reads as writes not enabled", async () => {
    const { fetch } = recordingFetch(() => new Response("boom", { status: 500 }));
    expect(await fetchConsoleConfig(httpFor(fetch))).toEqual(disabled);
  });

  test("a network failure reads as writes not enabled, never a throw", async () => {
    const { fetch } = recordingFetch(() => {
      throw new TypeError("network down");
    });
    expect(await fetchConsoleConfig(httpFor(fetch))).toEqual(disabled);
  });

  test("an undecodable body reads as writes not enabled", async () => {
    for (const body of ["not json", "[]", "null", '{"writes_enabled":"yes"}']) {
      const { fetch } = recordingFetch(() => json(body));
      expect(await fetchConsoleConfig(httpFor(fetch))).toEqual(disabled);
    }
  });
});

describe("listDaemons", () => {
  test("listDaemons sends the start token, which is what the server gates GET /daemons on", async () => {
    const { fetch, calls } = recordingFetch(() => json("[]"));
    await listDaemons(httpFor(fetch));
    expect(calls[0]?.url).toBe("/daemons");
    expect(calls[0]?.init.method).toBe("GET");
    expect(calls[0]?.init.headers).toEqual({ Authorization: "Bearer start-t" });
  });

  test("decodes the fixture", async () => {
    const { fetch } = recordingFetch(() => json(readFixtureText("api/daemons.json")));
    const daemons = await listDaemons(httpFor(fetch));
    expect(daemons).toHaveLength(1);
    expect(daemons[0]?.repository).toBe("acme/app");
    expect(daemons[0]?.state).toBe("running");
    expect(daemons[0]?.pid).toBe(4242);
    expect(daemons[0]?.heartbeatUpdatedAt).toBe("2026-09-10T09:00:00Z");
  });
});

describe("getQueueRunStatus", () => {
  test("getQueueRunStatus sends the read token, which is what the server gates GET /queue-run on", async () => {
    const { fetch, calls } = recordingFetch(() => json('{"state":"alive"}'));
    const status = await getQueueRunStatus(httpFor(fetch));
    expect(calls[0]?.url).toBe("/queue-run");
    expect(calls[0]?.init.method).toBe("GET");
    expect(calls[0]?.init.headers).toEqual({ Authorization: "Bearer read-t" });
    expect(status.state).toBe("alive");
  });

  test("decodes the fixture", async () => {
    const { fetch } = recordingFetch(() => json(readFixtureText("api/queue-run.json")));
    const status = await getQueueRunStatus(httpFor(fetch));
    expect(status).toEqual({ state: "stale", lastHeartbeat: "2026-09-10T09:50:00Z" });
  });
});

describe("listWorkspaces", () => {
  test("sends GET /workspaces with the read token and decodes the fixture", async () => {
    const { fetch, calls } = recordingFetch(() => json(readFixtureText("api/workspaces.json")));
    const workspaces = await listWorkspaces(httpFor(fetch));
    expect(calls[0]?.url).toBe("/workspaces");
    expect(calls[0]?.init.method).toBe("GET");
    expect(calls[0]?.init.headers).toEqual({ Authorization: "Bearer read-t" });
    expect(workspaces).toHaveLength(1);
    expect(workspaces[0]?.workspace).toBe("/repos/app");
    expect(workspaces[0]?.hasFactoryYml).toBe(false);
  });
});
