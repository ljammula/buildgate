import {
  newRequestRoute,
  opsRoute,
  parseRoute,
  pathForRoute,
  projectReleaseRoute,
  projectStatsRoute,
  requestDetailRoute,
  runDetailRoute,
  runListRoute,
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

test("/projects/{p}/release parses to a projectRelease deep link", () => {
  const config = parseRoute("/projects/checkouts/release");
  expect(config.deepLink).toBe("projectRelease");
  expect(config.deepLinkId).toBe("checkouts");
});

test("each deep link round-trips through pathForRoute", () => {
  expect(pathForRoute(requestDetailRoute("req-42"))).toBe("/requests/req-42");
  expect(pathForRoute(runDetailRoute("run-7"))).toBe("/runs/run-7");
  expect(pathForRoute(projectReleaseRoute("checkouts"))).toBe("/projects/checkouts/release");
});

test("a deep-link id with an escapable character round-trips without double-encoding", () => {
  const rawId = "my repo";
  const path = pathForRoute(projectReleaseRoute(rawId));
  expect(path).toBe("/projects/my%20repo/release");
  const config = parseRoute(path);
  expect(config.deepLink).toBe("projectRelease");
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

test("/ops parses to an ops deep link", () => {
  const config = parseRoute("/ops");
  expect(config.deepLink).toBe("ops");
  expect(config.deepLinkId).toBeNull();
});

test("/projects/{p}/stats parses to a projectStats deep link", () => {
  const config = parseRoute("/projects/checkouts/stats");
  expect(config.deepLink).toBe("projectStats");
  expect(config.deepLinkId).toBe("checkouts");
});

test("these deep links round-trip through pathForRoute", () => {
  expect(pathForRoute(runListRoute())).toBe("/runs");
  expect(pathForRoute(opsRoute())).toBe("/ops");
  expect(pathForRoute(projectStatsRoute("checkouts"))).toBe("/projects/checkouts/stats");
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
  expect(config.filters.projects).toEqual(["checkouts"]);
});
