import { useProjectOpsQueries, useProjects } from "@/api/runQueries";
import { ApiError } from "@/domain/apiError";
import type { ProjectStats, ProjectSummary } from "@/domain/project";
import type { ProjectReleaseView } from "@/domain/release";

export interface ProjectOpsRow {
  readonly summary: ProjectSummary;
  /** Null when this project's stats fetch failed. */
  readonly stats: ProjectStats | null;
  /** Null when this project's release fetch failed: the kill switch is then unknown, never clear. */
  readonly release: ProjectReleaseView | null;
}

function isAuthFailure(error: unknown): boolean {
  return error instanceof ApiError && (error.status === 401 || error.status === 403);
}

/**
 * Every known project with its own stats and kill-switch state. Each
 * project's stats/release failure is isolated to its row (a slow or
 * unreachable project must not sink the whole page).
 */
export function useProjectOps() {
  const projects = useProjects();
  const summaries = projects.data ?? [];
  const queries = useProjectOpsQueries(summaries.map((summary) => summary.project));

  const rows: ProjectOpsRow[] = summaries.flatMap((summary) => {
    const pair = queries.find((q) => q.project === summary.project);
    return pair === undefined
      ? []
      : [{ summary, stats: pair.stats.data ?? null, release: pair.release.data ?? null }];
  });
  const results = queries.flatMap((q) => [q.stats, q.release]);
  const loading = projects.isPending || results.some((result) => result.isPending);
  // 401/403 on a start-token-gated route: a stale/missing start token, not a
  // broken project, is the likely cause, so it is surfaced once.
  const startTokenFailure = results.some((result) => isAuthFailure(result.error));

  const refresh = async () => {
    await Promise.all([projects.refetch(), ...results.map((result) => result.refetch())]);
  };
  return { projects, rows, loading, startTokenFailure, refresh };
}
