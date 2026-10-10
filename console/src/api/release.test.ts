import { createHttp } from "@/api/http";
import { getProjectRelease, getRunRelease } from "@/api/release";
import { readFixtureText } from "@/test/fixtures";

interface Call {
  readonly url: string;
  readonly init: RequestInit;
}

function recordingFetch(respond: (call: Call) => Response) {
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

describe("getRunRelease", () => {
  test("getRunRelease parses decided, undecided, and omitempty shapes", async () => {
    const allowed = JSON.stringify({
      run_id: "run-clean",
      project: "checkouts",
      decision: {
        run_id: "run-clean",
        project: "checkouts",
        allowed: true,
        evaluated_at: "2026-09-03T10:05:00Z",
      },
      kill_switch: { project: "checkouts", engaged: false, history: null },
    });
    const { fetch, calls } = recordingFetch((call) => {
      if (call.url.endsWith("/run-quarantined/release")) {
        return json(readFixtureText("api/run-release-no-decision.json"));
      }
      if (call.url.endsWith("/run-clean/release")) return json(allowed);
      return json(readFixtureText("api/run-release.json"));
    });
    const http = httpFor(fetch, "http://factory.test");

    const denied = await getRunRelease(http, "run-accepted");
    expect(calls[0]?.url).toBe("http://factory.test/runs/run-accepted/release");
    expect(denied.project).toBe("app");
    expect(denied.decision?.allowed).toBe(false);
    expect(denied.decision?.reasons[0]).toContain("kill switch is engaged");
    expect(denied.decision?.evaluatedAt).toBe("2026-09-10T09:27:00Z");
    expect(denied.killSwitch.engaged).toBe(true);
    expect(denied.killSwitch.history[0]?.by).toBe("operator@example.com");
    expect(denied.killSwitch.history[0]?.reason).toBe("incident 42");

    // An allowed decision omits "reasons" entirely and a never-engaged switch
    // serializes a null history: both must decode as empty, not throw.
    const clean = await getRunRelease(http, "run-clean");
    expect(clean.decision?.allowed).toBe(true);
    expect(clean.decision?.reasons).toEqual([]);
    expect(clean.killSwitch.engaged).toBe(false);
    expect(clean.killSwitch.history).toEqual([]);

    // A run with no decision recorded decodes as null, never as allowed.
    const undecided = await getRunRelease(http, "run-quarantined");
    expect(undecided.decision).toBeNull();
  });

  test("sends GET with the start token, not the read token", async () => {
    const { fetch, calls } = recordingFetch(() => json(readFixtureText("api/run-release.json")));
    await getRunRelease(httpFor(fetch), "run/1");
    expect(calls).toHaveLength(1);
    expect(calls[0]?.url).toBe("/runs/run%2F1/release");
    expect(calls[0]?.init.method).toBe("GET");
    expect(calls[0]?.init.headers).toEqual({ Authorization: "Bearer start-t" });
    expect(calls[0]?.init.body).toBeUndefined();
  });

  test("a 403 (no start token configured server-side) rejects with status 403", async () => {
    const { fetch } = recordingFetch(() => json('{"error":"not authorized"}', 403));
    await expect(getRunRelease(httpFor(fetch), "run-1")).rejects.toMatchObject({ status: 403 });
  });
});

describe("getProjectRelease", () => {
  test("sends GET to the encoded project with the start token and decodes the fixture", async () => {
    const { fetch, calls } = recordingFetch(() =>
      json(readFixtureText("api/project-release.json")),
    );
    const view = await getProjectRelease(httpFor(fetch), "my app");
    expect(calls[0]?.url).toBe("/projects/my%20app/release");
    expect(calls[0]?.init.method).toBe("GET");
    expect(calls[0]?.init.headers).toEqual({ Authorization: "Bearer start-t" });
    expect(view.project).toBe("app");
    expect(view.killSwitch.engaged).toBe(true);
    expect(view.killSwitch.history[0]?.at).toBe("2026-09-10T09:26:00Z");
  });
});
