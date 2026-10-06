import { checkProject, getProjectStats, listProjects } from "@/api/projects";
import { createHttp } from "@/api/http";
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

const httpFor = (fetch: typeof globalThis.fetch) =>
  createHttp({
    baseUrl: "",
    readToken: "read-t",
    startToken: "start-t",
    overrideToken: "override-t",
    fetch,
  });

const json = (body: string, status = 200) =>
  new Response(body, { status, headers: { "content-type": "application/json" } });

describe("listProjects", () => {
  test("sends GET /projects with the read token and decodes the fixture", async () => {
    const { fetch, calls } = recordingFetch(() => json(readFixtureText("api/projects.json")));
    const projects = await listProjects(httpFor(fetch));
    expect(calls).toHaveLength(1);
    expect(calls[0]?.url).toBe("/projects");
    expect(calls[0]?.init.method).toBe("GET");
    expect(calls[0]?.init.headers).toEqual({ Authorization: "Bearer read-t" });
    expect(projects).toHaveLength(1);
    expect(projects[0]?.project).toBe("app");
    expect(projects[0]?.runCount).toBe(4);
    expect(projects[0]?.lastRunAt).toBe("2026-09-10T09:45:00Z");
    expect(projects[0]?.repository).toBe("");
  });

  test("read routes send no Authorization header when no read token is configured", async () => {
    const { fetch, calls } = recordingFetch(() => json("[]"));
    const http = createHttp({
      baseUrl: "http://factory.test",
      readToken: null,
      startToken: null,
      overrideToken: null,
      fetch,
    });
    await listProjects(http);
    expect(calls[0]?.url).toBe("http://factory.test/projects");
    expect(calls[0]?.init.headers).toEqual({});
  });
});

describe("checkProject", () => {
  const passed =
    '{"passed":true,"checks":[{"check":"spec","path":"p","passed":true,"reasons":[]}]}';

  test("posts workspace, repository and a non-empty ticket with the start token", async () => {
    const { fetch, calls } = recordingFetch(() => json(passed));
    const result = await checkProject(httpFor(fetch), {
      workspace: "/repos/app",
      repository: "acme/app",
      ticket: "t-1",
    });
    expect(result.passed).toBe(true);
    expect(result.checks[0]?.check).toBe("spec");
    expect(calls[0]?.url).toBe("/projects/check");
    expect(calls[0]?.init.method).toBe("POST");
    expect(calls[0]?.init.headers).toEqual({
      "Content-Type": "application/json",
      Authorization: "Bearer start-t",
    });
    expect(JSON.parse(calls[0]?.init.body as string)).toEqual({
      workspace: "/repos/app",
      repository: "acme/app",
      ticket: "t-1",
    });
  });

  test("omits the ticket key when the ticket is empty or absent", async () => {
    const { fetch, calls } = recordingFetch(() => json(passed));
    const http = httpFor(fetch);
    await checkProject(http, { workspace: "/w", repository: "r", ticket: "" });
    await checkProject(http, { workspace: "/w", repository: "r" });
    for (const call of calls) {
      expect(JSON.parse(call.init.body as string)).toEqual({ workspace: "/w", repository: "r" });
    }
  });

  test("a 403 rejects with an ApiError", async () => {
    const { fetch } = recordingFetch(() =>
      json('{"error":"start endpoint is not authorized"}', 403),
    );
    await expect(
      checkProject(httpFor(fetch), { workspace: "/w", repository: "r" }),
    ).rejects.toMatchObject({ status: 403, serverMessage: "start endpoint is not authorized" });
  });
});

describe("getProjectStats", () => {
  test("sends GET to the encoded project with the start token and decodes the fixture", async () => {
    const { fetch, calls } = recordingFetch(() => json(readFixtureText("api/project-stats.json")));
    const stats = await getProjectStats(httpFor(fetch), "app");
    expect(calls[0]?.url).toBe("/projects/app/stats");
    expect(calls[0]?.init.method).toBe("GET");
    expect(calls[0]?.init.headers).toEqual({ Authorization: "Bearer start-t" });
    expect(stats.project).toBe("app");
    expect(stats.totalRuns).toBe(3);
    expect(stats.overrideRatePercent).toBe(3);
    expect(stats.quarantinedByCause).toEqual({ "every-field key": 3 });
    expect(stats.medianAcceptedCostSubscriptionBilled).toBe(true);
  });

  test("encodes a project id with reserved characters", async () => {
    const { fetch, calls } = recordingFetch(() => json(readFixtureText("api/project-stats.json")));
    await getProjectStats(httpFor(fetch), "a b/c");
    expect(calls[0]?.url).toBe("/projects/a%20b%2Fc/stats");
  });
});
