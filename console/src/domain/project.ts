// Projects as the API lists them and the per-project figures a team lead
// decides on: GET /projects, GET /projects/{project}/stats and
// POST /projects/check.
import { NO_VALUE } from "@/domain/noValue";
import {
  type JsonObject,
  decodeList,
  numberMap,
  objectList,
  optBoolean,
  optNumber,
  optString,
  reqBoolean,
  reqNumber,
  reqString,
  stringList,
} from "@/domain/decode";

/** internal/api.ProjectSummary: one entry of GET /projects. */
export interface ProjectSummary {
  readonly projectPath: string;
  /**
   * The release-decision/kill-switch project identifier, distinct from
   * `projectPath` (a filesystem path). Surfaced so an operator can discover
   * the exact value `factoryd kill-switch -project` expects instead of
   * inspecting raw JSON (found via a real GitHub Codex App review).
   */
  readonly project: string;
  readonly workspacePath: string;
  readonly specPath: string;
  /**
   * Empty, not null, when the most recent run against this project never
   * used one. Found via review: without it, selecting a known project only
   * prefilled Workspace/Spec and left Repository (required whenever a run
   * needs the shared per-repository task queue) to be retyped every time.
   */
  readonly repository: string;
  readonly runCount: number;
  readonly lastRunAt: string;
}

function decodeProjectSummary(o: JsonObject, at: string): ProjectSummary {
  return {
    projectPath: reqString(o, "project_path", at),
    project: reqString(o, "project", at),
    workspacePath: reqString(o, "workspace_path", at),
    specPath: reqString(o, "spec_path", at),
    repository: optString(o, "repository", at),
    runCount: reqNumber(o, "run_count", at),
    lastRunAt: reqString(o, "last_run_at", at),
  };
}

export function decodeProjectList(value: unknown, at: string): ProjectSummary[] {
  return decodeList(value, at, decodeProjectSummary);
}

/**
 * internal/api.ProjectStats: GET /projects/{project}/stats, the per-team
 * acceptance-rate figures a team lead decides whether to route real tickets
 * here on. The nullable figures are null, not 0, for a project with no
 * accepted runs: "no data" must render differently from "a real 0%/$0".
 */
export interface ProjectStats {
  readonly project: string;
  readonly totalRuns: number;
  readonly accepted: number;
  readonly acceptedViaOverride: number;
  readonly overrideRatePercent: number | null;
  readonly quarantinedByCause: Readonly<Record<string, number>>;
  readonly halted: number;
  readonly medianAcceptedCostMicroUsd: number | null;
  /**
   * True when at least one accepted run behind the median cost was billed to
   * a ChatGPT/Copilot subscription rather than a metered API key. An "any"
   * treatment, not a per-run breakdown: this aggregate mixes runs across the
   * whole project.
   */
  readonly medianAcceptedCostSubscriptionBilled: boolean;
  /**
   * Median total relay token count (input + output) across accepted runs,
   * computed like the median cost: null for a project with no accepted runs,
   * never a misleading 0.
   */
  readonly medianAcceptedTokens: number | null;
}

export function decodeProjectStats(o: JsonObject, at: string): ProjectStats {
  return {
    project: reqString(o, "project", at),
    totalRuns: reqNumber(o, "total_runs", at),
    accepted: reqNumber(o, "accepted", at),
    acceptedViaOverride: reqNumber(o, "accepted_via_override", at),
    overrideRatePercent: optNumber(o, "override_rate_percent", at),
    quarantinedByCause: numberMap(o, "quarantined_by_cause", at),
    halted: reqNumber(o, "halted", at),
    medianAcceptedCostMicroUsd: optNumber(o, "median_accepted_cost_micro_usd", at),
    medianAcceptedCostSubscriptionBilled: optBoolean(
      o,
      "median_accepted_cost_subscription_billed",
      at,
    ),
    medianAcceptedTokens: optNumber(o, "median_accepted_tokens", at),
  };
}

/**
 * "2 / 4", plus " · 50% via override" when the server gives an override rate;
 * the no-value dash when the stats could not be read, never "0 / 0".
 */
export function acceptedText(stats: ProjectStats | null): string {
  if (stats === null) return NO_VALUE;
  const base = `${stats.accepted} / ${stats.totalRuns}`;
  return stats.overrideRatePercent === null
    ? base
    : `${base} · ${stats.overrideRatePercent}% via override`;
}

/**
 * internal/api.ProjectCheckResult: one project-bootstrap structural check's
 * verdict, from POST /projects/check.
 */
export interface ProjectCheckResult {
  readonly check: string;
  readonly path: string;
  readonly passed: boolean;
  readonly reasons: readonly string[];
}

function decodeProjectCheckResult(o: JsonObject, at: string): ProjectCheckResult {
  return {
    check: reqString(o, "check", at),
    path: reqString(o, "path", at),
    passed: reqBoolean(o, "passed", at),
    reasons: stringList(o, "reasons", at),
  };
}

/**
 * POST /projects/check's response: the console's "check project setup"
 * preview of the same project-bootstrap preflight a real run would otherwise
 * fail closed on, without ever starting one.
 */
export interface ProjectCheckResponse {
  readonly passed: boolean;
  readonly checks: readonly ProjectCheckResult[];
}

export function decodeProjectCheckResponse(o: JsonObject, at: string): ProjectCheckResponse {
  return {
    passed: reqBoolean(o, "passed", at),
    checks: objectList(o, "checks", at, decodeProjectCheckResult),
  };
}
