// The run, project and operations hooks screens use.
import { type UseQueryResult, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useState } from "react";

import { useApi } from "@/api/ApiProvider";
import { getQueueRunStatus, listDaemons, listWorkspaces } from "@/api/ops";
import {
  type CheckProjectInput,
  checkProject,
  getProjectStats,
  listProjects,
} from "@/api/projects";
import { queryKeys } from "@/api/queryKeys";
import { getProjectRelease, getRunRelease } from "@/api/release";
import {
  type OverrideRunInput,
  type StartRunInput,
  getRun,
  getRunDiff,
  listRuns,
  overrideRun,
  startRun,
  watchRun,
} from "@/api/runs";
import type { ApiError } from "@/domain/apiError";
import type { DaemonStatus, QueueRunStatus, WorkspaceHint } from "@/domain/ops";
import type { ProjectCheckResponse, ProjectStats, ProjectSummary } from "@/domain/project";
import type { ProjectReleaseView, ReleaseView } from "@/domain/release";
import { type Run, type RunDiff, runIsTerminal } from "@/domain/run";

export function useRuns(refetchIntervalMs?: number): UseQueryResult<Run[]> {
  const { http } = useApi();
  return useQuery({
    queryKey: queryKeys.runs.list(),
    queryFn: ({ signal }) => listRuns(http, signal),
    ...(refetchIntervalMs === undefined ? {} : { refetchInterval: refetchIntervalMs }),
  });
}

/**
 * Whichever record of one run is newer; `updated_at` is RFC 3339 UTC, so it
 * orders as text.
 */
export function newerRun(cached: Run | undefined, incoming: Run): Run {
  return cached && cached.updatedAt > incoming.updatedAt ? cached : incoming;
}

export interface LiveRun {
  readonly query: UseQueryResult<Run>;
  /** A permanent stream failure (a 4xx: a rotated token, a pruned run). */
  readonly streamError: ApiError | null;
}

/**
 * One run, kept current by `GET /runs/{id}/events` until it is terminal.
 * The stream's values go into the same cache entry the fetch fills, so a
 * screen reads one `query.data` whichever delivered it.
 */
export function useRun(id: string): LiveRun {
  const { http } = useApi();
  const client = useQueryClient();
  const [streamError, setStreamError] = useState<ApiError | null>(null);

  const query = useQuery({
    queryKey: queryKeys.runs.detail(id),
    queryFn: ({ signal }) => getRun(http, id, signal),
    // A refetch that was already in flight must not replace a newer record
    // the stream delivered meanwhile.
    structuralSharing: (cached, fetched) => newerRun(cached as Run | undefined, fetched as Run),
  });

  // The stream opens once the run is known and only while it can still
  // change: a finished run makes no events request at all.
  const watching = query.data !== undefined && !runIsTerminal(query.data);
  useEffect(() => {
    if (!watching) return;
    const unsubscribe = watchRun(http, id, {
      onValue: (run) => {
        client.setQueryData<Run>(queryKeys.runs.detail(id), (cached) => newerRun(cached, run));
        if (runIsTerminal(run)) {
          // The run's evidence (diff, release decision) is final only now.
          void client.invalidateQueries({
            queryKey: queryKeys.runs.detail(id),
            predicate: (q) => q.queryKey.length > 3,
          });
        }
      },
      onError: setStreamError,
    });
    return unsubscribe;
  }, [http, client, id, watching]);

  return { query, streamError };
}

export function useRunDiff(id: string, enabled = true): UseQueryResult<RunDiff> {
  const { http } = useApi();
  return useQuery({
    queryKey: queryKeys.runs.diff(id),
    queryFn: ({ signal }) => getRunDiff(http, id, signal),
    enabled,
  });
}

export function useRunRelease(id: string, enabled = true): UseQueryResult<ReleaseView> {
  const { http } = useApi();
  return useQuery({
    queryKey: queryKeys.runs.release(id),
    queryFn: ({ signal }) => getRunRelease(http, id, signal),
    enabled,
  });
}

export function useStartRun() {
  const { http } = useApi();
  const client = useQueryClient();
  return useMutation<Run, ApiError, StartRunInput>({
    mutationFn: (input) => startRun(http, input),
    onSuccess: (run) => {
      client.setQueryData(queryKeys.runs.detail(run.id), run);
      void client.invalidateQueries({ queryKey: queryKeys.runs.list() });
      void client.invalidateQueries({ queryKey: queryKeys.projects.all });
    },
  });
}

export function useOverrideRun(id: string) {
  const { http } = useApi();
  const client = useQueryClient();
  return useMutation<Run, ApiError, OverrideRunInput>({
    mutationFn: (input) => overrideRun(http, id, input),
    onSuccess: (run) => {
      client.setQueryData(queryKeys.runs.detail(run.id), run);
      // An override changes the run's release decision and its request.
      void client.invalidateQueries({
        queryKey: queryKeys.runs.detail(id),
        predicate: (q) => q.queryKey.length > 3,
      });
      void client.invalidateQueries({ queryKey: queryKeys.runs.list() });
      void client.invalidateQueries({ queryKey: queryKeys.requests.all });
      void client.invalidateQueries({ queryKey: queryKeys.projects.all });
    },
  });
}

export function useProjects(): UseQueryResult<ProjectSummary[]> {
  const { http } = useApi();
  return useQuery({
    queryKey: queryKeys.projects.list(),
    queryFn: ({ signal }) => listProjects(http, signal),
  });
}

export function useProjectStats(project: string): UseQueryResult<ProjectStats> {
  const { http } = useApi();
  return useQuery({
    queryKey: queryKeys.projects.stats(project),
    queryFn: ({ signal }) => getProjectStats(http, project, signal),
  });
}

export function useProjectRelease(project: string): UseQueryResult<ProjectReleaseView> {
  const { http } = useApi();
  return useQuery({
    queryKey: queryKeys.projects.release(project),
    queryFn: ({ signal }) => getProjectRelease(http, project, signal),
  });
}

export function useCheckProject() {
  const { http } = useApi();
  return useMutation<ProjectCheckResponse, ApiError, CheckProjectInput>({
    mutationFn: (input) => checkProject(http, input),
  });
}

export function useDaemons(refetchIntervalMs?: number): UseQueryResult<DaemonStatus[]> {
  const { http } = useApi();
  return useQuery({
    queryKey: queryKeys.ops.daemons(),
    queryFn: ({ signal }) => listDaemons(http, signal),
    ...(refetchIntervalMs === undefined ? {} : { refetchInterval: refetchIntervalMs }),
  });
}

export function useQueueRunStatus(refetchIntervalMs?: number): UseQueryResult<QueueRunStatus> {
  const { http } = useApi();
  return useQuery({
    queryKey: queryKeys.ops.queueRun(),
    queryFn: ({ signal }) => getQueueRunStatus(http, signal),
    ...(refetchIntervalMs === undefined ? {} : { refetchInterval: refetchIntervalMs }),
  });
}

export function useWorkspaces(): UseQueryResult<WorkspaceHint[]> {
  const { http } = useApi();
  return useQuery({
    queryKey: queryKeys.ops.workspaces(),
    queryFn: ({ signal }) => listWorkspaces(http, signal),
  });
}
