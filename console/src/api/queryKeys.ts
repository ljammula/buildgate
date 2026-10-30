// Every query key, in one place: a screen never builds one, and a mutation
// invalidates by the same constructors its queries were keyed with.
export const queryKeys = {
  requests: {
    all: ["requests"] as const,
    list: () => ["requests", "list"] as const,
    detail: (id: string) => ["requests", "detail", id] as const,
    revisions: (id: string) => ["requests", "detail", id, "revisions"] as const,
    revision: (id: string, index: number) =>
      ["requests", "detail", id, "revisions", index] as const,
  },
  runs: {
    all: ["runs"] as const,
    list: () => ["runs", "list"] as const,
    detail: (id: string) => ["runs", "detail", id] as const,
    /** The prefix of everything a run's end state settles: its diff and release decision. */
    evidence: (id: string) => ["runs", "detail", id, "evidence"] as const,
    diff: (id: string) => ["runs", "detail", id, "evidence", "diff"] as const,
    release: (id: string) => ["runs", "detail", id, "evidence", "release"] as const,
    handoff: (id: string, sha256: string) =>
      ["runs", "detail", id, "evidence", "handoff", sha256] as const,
    prompts: (id: string) => ["runs", "detail", id, "evidence", "prompts"] as const,
    stepLog: (id: string, step: string) =>
      ["runs", "detail", id, "evidence", "step", step] as const,
    promptText: (id: string, attempt: string, name: string) =>
      ["runs", "detail", id, "evidence", "prompts", attempt, name] as const,
  },
  projects: {
    all: ["projects"] as const,
    list: () => ["projects", "list"] as const,
    stats: (project: string) => ["projects", project, "stats"] as const,
    observations: (project: string) => ["projects", project, "observations"] as const,
    memory: (project: string) => ["projects", project, "memory"] as const,
    trend: (project: string) => ["projects", project, "trend"] as const,
    release: (project: string) => ["projects", project, "release"] as const,
  },
  ops: {
    daemons: () => ["ops", "daemons"] as const,
    queueRun: () => ["ops", "queue-run"] as const,
    workspaces: () => ["ops", "workspaces"] as const,
    /** One entry per window: `since` is "7d", "30d" or null for all time. */
    stats: (since: string | null) => ["ops", "stats", since ?? "all"] as const,
  },
};

/** The segment after `["requests", "detail", id]` that each kind of query under a request uses. */
const underRequestSegments: ReadonlySet<unknown> = new Set(["revisions"]);

/**
 * True for a query keyed under the request `id` but not the request's own
 * detail record: its revisions. Not the detail, not the list.
 */
export function isUnderRequest(key: readonly unknown[], id: string): boolean {
  return (
    key[0] === "requests" &&
    key[1] === "detail" &&
    key[2] === id &&
    underRequestSegments.has(key[3])
  );
}
