import { type Http } from "@/api/http";
import { asObject } from "@/domain/decode";
import {
  type ProjectReleaseView,
  type ReleaseView,
  decodeProjectReleaseView,
  decodeReleaseView,
} from "@/domain/release";

/**
 * Fetches one run's durable release decision and the project kill-switch
 * state it was evaluated against. Authenticated with the start token: the
 * server treats this as a control-plane route (like GET /daemons) rather
 * than an open read route, because the kill-switch history carries operator
 * attribution, and it fails closed (403) when no token is configured
 * server-side.
 */
export async function getRunRelease(
  http: Http,
  id: string,
  signal?: AbortSignal,
): Promise<ReleaseView> {
  const at = "GET /runs/{id}/release";
  const json = await http.getJson(`/runs/${encodeURIComponent(id)}/release`, "start", signal);
  return decodeReleaseView(asObject(json, at), at);
}

/**
 * GET /projects/{project}/release: a project's kill-switch state and history
 * by project id alone, no run required for that project to exist. It exists
 * alongside getRunRelease because a project whose kill switch is engaged but
 * which has no runs had no console surface at all. Start token.
 */
export async function getProjectRelease(
  http: Http,
  project: string,
  signal?: AbortSignal,
): Promise<ProjectReleaseView> {
  const at = "GET /projects/{project}/release";
  const json = await http.getJson(
    `/projects/${encodeURIComponent(project)}/release`,
    "start",
    signal,
  );
  return decodeProjectReleaseView(asObject(json, at), at);
}
