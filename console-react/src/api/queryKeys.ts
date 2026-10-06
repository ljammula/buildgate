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
    oracle: (id: string) => ["requests", "detail", id, "oracle"] as const,
    oracleFile: (id: string, name: string) =>
      ["requests", "detail", id, "oracle", "file", name] as const,
    ticketOracle: (id: string, ticket: number) =>
      ["requests", "detail", id, "tickets", ticket, "oracle"] as const,
    ticketOracleFile: (id: string, ticket: number, name: string) =>
      ["requests", "detail", id, "tickets", ticket, "oracle", "file", name] as const,
  },
  runs: {
    all: ["runs"] as const,
    list: () => ["runs", "list"] as const,
    detail: (id: string) => ["runs", "detail", id] as const,
    diff: (id: string) => ["runs", "detail", id, "diff"] as const,
    release: (id: string) => ["runs", "detail", id, "release"] as const,
  },
  projects: {
    all: ["projects"] as const,
    list: () => ["projects", "list"] as const,
    stats: (project: string) => ["projects", project, "stats"] as const,
    release: (project: string) => ["projects", project, "release"] as const,
  },
  ops: {
    daemons: () => ["ops", "daemons"] as const,
    queueRun: () => ["ops", "queue-run"] as const,
    workspaces: () => ["ops", "workspaces"] as const,
  },
};
