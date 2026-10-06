import { type Http } from "@/api/http";
import { asObject } from "@/domain/decode";
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
