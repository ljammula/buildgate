import { useProjectOpsQueries, useProjects } from "@/api/runQueries";
import { ApiError } from "@/domain/apiError";
import type { ProjectStats, ProjectSummary } from "@/domain/project";
import type { ProjectReleaseView } from "@/domain/release";

export interface ProjectRow {
  readonly summary: ProjectSummary;
  /** Null when this project's stats read failed, even if an earlier read left data. */
  readonly stats: ProjectStats | null;
  /**
   * Null when this project's release read failed, even if an earlier read left data: the kill
   * switch is then unknown, never an old state shown as current.
   */
  readonly release: ProjectReleaseView | null;
  /** The latest stats read failed. */
  readonly statsFailed: boolean;
  /** The latest release read failed. */
  readonly releaseFailed: boolean;
  /** True while either read has no answer yet: the row shows a dash, not a guess. */
  readonly loading: boolean;
}

function isAuthFailure(error: unknown): boolean {
  return error instanceof ApiError && (error.status === 401 || error.status === 403);
}

/**
 * Every known project with its own stats and kill-switch state. Each
 * project's stats/release failure is isolated to its row (a slow or
 * unreachable project must not sink the whole page).
 */
export function useProjectRows() {
  const projects = useProjects();
  const summaries = projects.data ?? [];
  const queries = useProjectOpsQueries(summaries.map((summary) => summary.project));

  const rows: ProjectRow[] = summaries.flatMap((summary) => {
    const pair = queries.find((q) => q.project === summary.project);
    return pair === undefined
      ? []
      : [
          {
            summary,
            stats: pair.stats.isError ? null : (pair.stats.data ?? null),
            release: pair.release.isError ? null : (pair.release.data ?? null),
            statsFailed: pair.stats.isError,
            releaseFailed: pair.release.isError,
            loading: pair.stats.isPending || pair.release.isPending,
          },
        ];
  });
  const results = queries.flatMap((q) => [q.stats, q.release]);
  // 401/403 on a start-token-gated route: a stale/missing start token, not a
  // broken project, is the likely cause, so it is surfaced once.
  const startTokenFailure = results.some((result) => isAuthFailure(result.error));

  const refresh = async () => {
    await Promise.all([projects.refetch(), ...results.map((result) => result.refetch())]);
  };
  return { projects, rows, startTokenFailure, refresh };
}
