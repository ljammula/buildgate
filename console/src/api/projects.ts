import { type Http } from "@/api/http";
import { asObject } from "@/domain/decode";
import { type ProjectMemory, decodeProjectMemory } from "@/domain/memory";
import { type FactoryStats, decodeFactoryStats } from "@/domain/stats";
import { type ProjectTrend, decodeProjectTrend } from "@/domain/trend";
import { type ObservationReport, decodeObservationReport } from "@/domain/observation";
import {
  type ProjectCheckResponse,
  type ProjectStats,
  type ProjectSummary,
  decodeProjectCheckResponse,
  decodeProjectList,
  decodeProjectStats,
} from "@/domain/project";

export async function listProjects(http: Http, signal?: AbortSignal): Promise<ProjectSummary[]> {
  return decodeProjectList(await http.getJson("/projects", "read", signal), "GET /projects");
}

export interface CheckProjectInput {
  readonly workspace: string;
  readonly repository: string;
  /** Optional: omitted from the body when empty. */
  readonly ticket?: string;
}

/**
 * Previews the project-bootstrap preflight that startRun's own check would
 * otherwise fail closed on, without starting a run or persisting anything.
 * The ticket is optional: a project with no ticket drafted yet still gets a
 * verdict on the three project-level artifacts alone. Same start-token
 * boundary and repository scope as startRun (see internal/api's checkProject
 * doc comment for why this is not treated as a plain read).
 */
export async function checkProject(
  http: Http,
  input: CheckProjectInput,
): Promise<ProjectCheckResponse> {
  const at = "POST /projects/check";
  const ticket = input.ticket ?? "";
  const json = await http.sendJson("POST", "/projects/check", "start", {
    workspace: input.workspace,
    repository: input.repository,
    ...(ticket !== "" ? { ticket } : {}),
  });
  return decodeProjectCheckResponse(asObject(json, at), at);
}

/**
 * GET /projects/{project}/stats: per-team acceptance and override rate,
 * quarantine-cause breakdown and median accepted cost, by project id alone.
 * Start token, as the server gates it.
 */
export async function getProjectStats(
  http: Http,
  project: string,
  signal?: AbortSignal,
): Promise<ProjectStats> {
  const at = "GET /projects/{project}/stats";
  const json = await http.getJson(
    `/projects/${encodeURIComponent(project)}/stats`,
    "start",
    signal,
  );
  return decodeProjectStats(asObject(json, at), at);
}

/**
 * GET /projects/{project}/observations: what the project's finished runs say
 * happened, computed from the run records. Read token, like the runs it is
 * made from.
 */
export async function getProjectObservations(
  http: Http,
  project: string,
  signal?: AbortSignal,
): Promise<ObservationReport> {
  const at = "GET /projects/{project}/observations";
  const json = await http.getJson(
    `/projects/${encodeURIComponent(project)}/observations`,
    "read",
    signal,
  );
  return decodeObservationReport(asObject(json, at), at);
}

/**
 * GET /projects/{project}/memory: whether repository memory is on, the lines
 * in force and the candidate lines. Read token; the route changes nothing.
 */
export async function getProjectMemory(
  http: Http,
  project: string,
  signal?: AbortSignal,
): Promise<ProjectMemory> {
  const at = "GET /projects/{project}/memory";
  const json = await http.getJson(
    `/projects/${encodeURIComponent(project)}/memory`,
    "read",
    signal,
  );
  return decodeProjectMemory(asObject(json, at), at);
}

/**
 * GET /projects/{project}/trend: whether the factory is getting better on one
 * repository, overall and per bucket of days. Read token; the route changes
 * nothing and calls no model.
 */
export async function getProjectTrend(
  http: Http,
  project: string,
  signal?: AbortSignal,
): Promise<ProjectTrend> {
  const at = "GET /projects/{project}/trend";
  const json = await http.getJson(`/projects/${encodeURIComponent(project)}/trend`, "read", signal);
  return decodeProjectTrend(asObject(json, at), at);
}

/**
 * GET /stats: the trend report over every project and one per project, for
 * Mission Control's numbers. `since` is the window in the form the trend
 * route takes ("7d", "30d"); null leaves it out, which is all time. Read
 * token, unlike the start-token-gated per-project stats route; it changes
 * nothing and calls no model.
 */
export async function getStats(
  http: Http,
  since: string | null,
  signal?: AbortSignal,
): Promise<FactoryStats> {
  const at = "GET /stats";
  const path = since === null ? "/stats" : `/stats?since=${encodeURIComponent(since)}`;
  return decodeFactoryStats(asObject(await http.getJson(path, "read", signal), at), at);
}
