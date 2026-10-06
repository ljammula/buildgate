import { DecodeError, asObject } from "@/domain/decode";
import {
  decodeProjectCheckResponse,
  decodeProjectList,
  decodeProjectStats,
} from "@/domain/project";
import { readFixtureJson } from "@/test/fixtures";

test("GET /projects decodes", () => {
  const projects = decodeProjectList(readFixtureJson("api/projects.json"), "GET /projects");
  expect(projects).toEqual([
    {
      projectPath: "/repos/app",
      project: "app",
      workspacePath: "/repos/app",
      specPath: "/data/requests/req-building/tickets/001.spec.md",
      repository: "",
      runCount: 4,
      lastRunAt: "2026-09-10T09:45:00Z",
    },
  ]);
});

test("a project missing a required field names the route and the field", () => {
  const bad = [{ project_path: "/repos/app", workspace_path: "/repos/app" }];
  expect(() => decodeProjectList(bad, "GET /projects")).toThrow(DecodeError);
  expect(() => decodeProjectList(bad, "GET /projects")).toThrow(/GET \/projects\[0\]\.project:/);
});

test("GET /projects/{project}/stats decodes every field", () => {
  const stats = decodeProjectStats(
    asObject(readFixtureJson("api/project-stats.json"), "project-stats.json"),
    "GET /projects/{project}/stats",
  );
  expect(stats.project).toBe("app");
  expect(stats.totalRuns).toBe(3);
  expect(stats.accepted).toBe(3);
  expect(stats.acceptedViaOverride).toBe(3);
  expect(stats.overrideRatePercent).toBe(3);
  expect(stats.quarantinedByCause).toEqual({ "every-field key": 3 });
  expect(stats.halted).toBe(3);
  expect(stats.medianAcceptedCostMicroUsd).toBe(3);
  expect(stats.medianAcceptedCostSubscriptionBilled).toBe(true);
  expect(stats.medianAcceptedTokens).toBe(3);
});

test("a project with no accepted runs keeps no-data figures null, not 0", () => {
  const stats = decodeProjectStats(
    {
      project: "p",
      total_runs: 1,
      accepted: 0,
      accepted_via_override: 0,
      halted: 1,
    },
    "GET /projects/{project}/stats",
  );
  expect(stats.overrideRatePercent).toBeNull();
  expect(stats.medianAcceptedCostMicroUsd).toBeNull();
  expect(stats.medianAcceptedTokens).toBeNull();
  expect(stats.medianAcceptedCostSubscriptionBilled).toBe(false);
  expect(stats.quarantinedByCause).toEqual({});
});

test("project stats missing a required field names the route and the field", () => {
  expect(() => decodeProjectStats({ project: "p" }, "GET /projects/{project}/stats")).toThrow(
    /GET \/projects\/\{project\}\/stats\.total_runs:/,
  );
});

test("POST /projects/check decodes checks, with absent reasons as an empty list", () => {
  const response = decodeProjectCheckResponse(
    {
      passed: false,
      checks: [
        { check: "factory-yml", path: "/repos/app", passed: true },
        { check: "oracle", path: "/repos/app", passed: false, reasons: ["no tests"] },
      ],
    },
    "POST /projects/check",
  );
  expect(response.passed).toBe(false);
  expect(response.checks).toEqual([
    { check: "factory-yml", path: "/repos/app", passed: true, reasons: [] },
    { check: "oracle", path: "/repos/app", passed: false, reasons: ["no tests"] },
  ]);
  expect(decodeProjectCheckResponse({ passed: true }, "POST /projects/check").checks).toEqual([]);
  expect(() => decodeProjectCheckResponse({ checks: [] }, "POST /projects/check")).toThrow(
    /POST \/projects\/check\.passed:/,
  );
});
