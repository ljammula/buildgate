// The run, project and operations hooks screens use.
import {
  type UseQueryResult,
  queryOptions,
  useMutation,
  useQueries,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query";
import { useEffect, useState } from "react";

import { useApi } from "@/api/ApiProvider";
import type { Http } from "@/api/http";
import { keepNewer } from "@/api/keepNewer";
import { getQueueRunStatus, listWorkspaces } from "@/api/ops";
import {
  type CheckProjectInput,
  checkProject,
  getProjectObservations,
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
import { compareTimestamps } from "@/domain/elapsed";
import type { ObservationReport } from "@/domain/observation";
import type { QueueRunStatus, WorkspaceHint } from "@/domain/ops";
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

/** Whichever record of one run is newer; a tie goes to the incoming one. */
function newerRun(cached: Run | undefined, incoming: Run): Run {
  return cached && compareTimestamps(cached.updatedAt, incoming.updatedAt) > 0 ? cached : incoming;
}

/**
 * One run's record (`GET /runs/{id}`): the one definition of the run detail
 * entry, whoever reads it. The event stream also writes this entry, so a
 * fetch that was already in flight must not replace a newer record.
 */
export function runDetailOptions(http: Http, id: string) {
  return queryOptions({
    queryKey: queryKeys.runs.detail(id),
    queryFn: ({ signal }) => getRun(http, id, signal),
    structuralSharing: keepNewer(newerRun),
  });
}

/**
 * One run with a plain fetch and no event stream: for a card that shows a
 * run it does not own (a ticket's run). Shares the cache entry `useRun`
 * keeps live. `enabled` defaults to true; `refetchIntervalMs` polls.
 */
export function useRunRecord(
  id: string,
  options: { enabled?: boolean; refetchIntervalMs?: number } = {},
): UseQueryResult<Run> {
  const { http } = useApi();
  return useQuery({
    ...runDetailOptions(http, id),
    enabled: options.enabled ?? true,
    ...(options.refetchIntervalMs === undefined
      ? {}
      : { refetchInterval: options.refetchIntervalMs }),
  });
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

  const query = useQuery(runDetailOptions(http, id));

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
          void client.invalidateQueries({ queryKey: queryKeys.runs.evidence(id) });
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
      void client.invalidateQueries({ queryKey: queryKeys.runs.evidence(id) });
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

function projectStatsOptions(http: Http, project: string) {
  return queryOptions({
    queryKey: queryKeys.projects.stats(project),
    queryFn: ({ signal }) => getProjectStats(http, project, signal),
  });
}

function projectReleaseOptions(http: Http, project: string) {
  return queryOptions({
    queryKey: queryKeys.projects.release(project),
    queryFn: ({ signal }) => getProjectRelease(http, project, signal),
  });
}

export function useProjectObservations(project: string): UseQueryResult<ObservationReport> {
  const { http } = useApi();
  return useQuery({
    queryKey: queryKeys.projects.observations(project),
    queryFn: ({ signal }) => getProjectObservations(http, project, signal),
  });
}

export function useProjectStats(project: string): UseQueryResult<ProjectStats> {
  const { http } = useApi();
  return useQuery(projectStatsOptions(http, project));
}

export function useProjectRelease(project: string): UseQueryResult<ProjectReleaseView> {
  const { http } = useApi();
  return useQuery(projectReleaseOptions(http, project));
}

/** One project's two reads, each with its own result so one failure stays on its row. */
export interface ProjectOpsQueries {
  readonly project: string;
  readonly stats: UseQueryResult<ProjectStats>;
  readonly release: UseQueryResult<ProjectReleaseView>;
}

/**
 * The stats and the release (kill switch) of every project in `projects`
 * (project names), in the same order. Two homogeneous `useQueries`, zipped,
 * so both results are typed without a cast or index arithmetic at the caller.
 */
export function useProjectOpsQueries(projects: readonly string[]): ProjectOpsQueries[] {
  const { http } = useApi();
  const stats = useQueries({ queries: projects.map((p) => projectStatsOptions(http, p)) });
  const release = useQueries({ queries: projects.map((p) => projectReleaseOptions(http, p)) });
  return projects.flatMap((project, index) => {
    const s = stats[index];
    const r = release[index];
    return s && r ? [{ project, stats: s, release: r }] : [];
  });
}

export function useCheckProject() {
  const { http } = useApi();
  return useMutation<ProjectCheckResponse, ApiError, CheckProjectInput>({
    mutationFn: (input) => checkProject(http, input),
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
