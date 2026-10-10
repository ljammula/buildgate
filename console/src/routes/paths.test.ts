import {
  isProjectTab,
  newRequestRoute,
  parseRoute,
  pathForRoute,
  projectsPath,
  requestDetailRoute,
  runDetailRoute,
  runListRoute,
  runOverridePath,
  parseRunOverrideReason,
} from "@/routes/paths";

test("/requests/{id} parses to a requestDetail deep link", () => {
  const config = parseRoute("/requests/req-42");
  expect(config.deepLink).toBe("requestDetail");
  expect(config.deepLinkId).toBe("req-42");
});

test("/runs/{id} parses to a runDetail deep link", () => {
  const config = parseRoute("/runs/run-7");
  expect(config.deepLink).toBe("runDetail");
  expect(config.deepLinkId).toBe("run-7");
});

test("each deep link round-trips through pathForRoute", () => {
  expect(pathForRoute(requestDetailRoute("req-42"))).toBe("/requests/req-42");
  expect(pathForRoute(runDetailRoute("run-7"))).toBe("/runs/run-7");
});

test("a deep-link id with an escapable character round-trips without double-encoding", () => {
  const rawId = "my repo";
  const path = pathForRoute(runDetailRoute(rawId));
  expect(path).toBe("/runs/my%20repo");
  const config = parseRoute(path);
  expect(config.deepLink).toBe("runDetail");
  expect(config.deepLinkId).toBe(rawId);
});

test('/requests/new parses to a newRequest deep link, not requestDetail(id: "new")', () => {
  const config = parseRoute("/requests/new");
  expect(config.deepLink).toBe("newRequest");
  expect(config.deepLinkId).toBeNull();
});

test("the newRequest deep link round-trips through pathForRoute", () => {
  expect(pathForRoute(newRequestRoute())).toBe("/requests/new");
});

test("/runs parses to a runList deep link", () => {
  const config = parseRoute("/runs");
  expect(config.deepLink).toBe("runList");
  expect(config.deepLinkId).toBeNull();
});

test("the run list deep link round-trips through pathForRoute", () => {
  expect(pathForRoute(runListRoute())).toBe("/runs");
});

test("projectsPath opens a project's row, on a tab when one is named", () => {
  expect(projectsPath()).toBe("/app/projects");
  expect(projectsPath("app")).toBe("/app/projects?project=app");
  expect(projectsPath("app", "release")).toBe("/app/projects?project=app&tab=release");
  expect(projectsPath("my repo", "stats")).toBe("/app/projects?project=my+repo&tab=stats");
  expect(projectsPath("")).toBe("/app/projects");
});

test("only the five tabs of a project row are tabs", () => {
  for (const tab of ["stats", "release", "trend", "observations", "memory"]) {
    expect(isProjectTab(tab)).toBe(true);
  }
  expect(isProjectTab("ops")).toBe(false);
  expect(isProjectTab(null)).toBe(false);
});

test("/triage still parses to the triage config, unaffected by the deep-link changes", () => {
  const config = parseRoute("/triage");
  expect(config.triage).toBe(true);
  expect(config.deepLink).toBe("none");
});

test("a bare / with filters still parses to the board config, unaffected by the deep-link changes", () => {
  const config = parseRoute("/?project=checkouts");
  expect(config.deepLink).toBe("none");
  expect(config.triage).toBe(false);
  expect(config.filters.projects).toEqual(new Set(["checkouts"]));
});

test("a run's override reason round-trips through its URL exactly once encoded", () => {
  const reason = "verify failed: 100% of rounds & more?";
  const url = new URL(runOverridePath("run 1", reason), "http://x.invalid");
  expect(url.pathname).toBe("/runs/run%201");
  expect(parseRunOverrideReason(url.search)).toBe(reason);
  expect(runOverridePath("run-1", "")).toBe("/runs/run-1");
  expect(parseRunOverrideReason("")).toBe("");
});
