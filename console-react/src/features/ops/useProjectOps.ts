import { useQueries } from "@tanstack/react-query";

import { useApi } from "@/api/ApiProvider";
import { queryKeys } from "@/api/queryKeys";
import { getProjectRelease } from "@/api/release";
import { getProjectStats } from "@/api/projects";
import { useProjects } from "@/api/runQueries";
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
  const { http } = useApi();
  const projects = useProjects();
  const summaries = projects.data ?? [];
  const results = useQueries({
    queries: summaries.flatMap((summary) => [
      {
        queryKey: queryKeys.projects.stats(summary.project),
        queryFn: ({ signal }: { signal: AbortSignal }) =>
          getProjectStats(http, summary.project, signal),
      },
      {
        queryKey: queryKeys.projects.release(summary.project),
        queryFn: ({ signal }: { signal: AbortSignal }) =>
          getProjectRelease(http, summary.project, signal),
      },
    ]),
  });

  const rows: ProjectOpsRow[] = summaries.map((summary, index) => {
    const stats = results[index * 2];
    const release = results[index * 2 + 1];
    return {
      summary,
      stats: (stats?.data as ProjectStats | undefined) ?? null,
      release: (release?.data as ProjectReleaseView | undefined) ?? null,
    };
  });
  const loading = projects.isPending || results.some((result) => result.isPending);
  // 401/403 on a start-token-gated route: a stale/missing start token, not a
  // broken project, is the likely cause, so it is surfaced once.
  const startTokenFailure = results.some((result) => isAuthFailure(result.error));

  const refresh = async () => {
    await Promise.all([projects.refetch(), ...results.map((result) => result.refetch())]);
  };
  return { projects, rows, loading, startTokenFailure, refresh };
}
