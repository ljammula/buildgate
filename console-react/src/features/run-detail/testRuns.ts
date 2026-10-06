// Run and release bodies the run-page tests serve, as objects a test can
// adjust before serving.

type Wire = Record<string, unknown>;

export function acceptedRun(): Wire {
  return {
    id: "run-accepted",
    ticket: "ticket-accepted",
    project_path: "/projects/app",
    workspace_path: "/workspaces/run-accepted",
    spec_path: "/specs/ticket-accepted.md",
    spec_sha256: "spec-accepted",
    state: "accepted",
    base_sha: "base-accepted",
    result_sha: "result-accepted",
    committed_by_factoryd: true,
    changed_files: ["lib/app.dart", "test/app_test.dart"],
    diff_stat: { files_changed: 2, insertions: 18, deletions: 3 },
    diff_available: true,
    attempts: [
      {
        command: ["python3", "build_app.py", "ticket-accepted"],
        started_at: "2026-08-26T11:00:00Z",
        finished_at: "2026-08-26T11:02:00Z",
        exit_code: 0,
        log_path: "/logs/attempt.log",
        role: "execution",
        harness: "pifork",
        thinking: "max",
        expected_effort: "max",
        relay_worker_model_id: "gpt-5.6-luna",
        relay_reasoning_effort: "high",
      },
    ],
    gate_results: [
      {
        check: "canonical_verify",
        command: ["make", "verify"],
        passed: true,
        exit_code: 0,
        duration_ms: 4200,
        log_sha256: "verify-log-hash",
      },
    ],
    notifications: [],
    overrides: [
      {
        by: "operator@example.com",
        reason: "Reviewed recovered evidence",
        at: "2026-08-26T11:04:00Z",
        prior_state: "quarantined",
        new_state: "accepted",
      },
    ],
    created_at: "2026-08-26T11:00:00Z",
    updated_at: "2026-08-26T11:04:00Z",
  };
}

export function quarantinedRun(): Wire {
  return {
    id: "run-quarantined",
    ticket: "ticket-quarantined",
    project_path: "/projects/app",
    workspace_path: "/workspaces/run-quarantined",
    spec_path: "/specs/ticket-quarantined.md",
    spec_sha256: "spec-quarantined",
    state: "quarantined",
    base_sha: "base-quarantined",
    committed_by_factoryd: false,
    changed_files: [],
    attempts: [],
    gate_results: [],
    notifications: [
      {
        run_id: "run-quarantined",
        ticket: "ticket-quarantined",
        reason: "canonical verification failed",
        state: "quarantined",
        sent_at: "2026-08-26T10:03:00Z",
      },
    ],
    overrides: [],
    created_at: "2026-08-26T10:00:00Z",
    updated_at: "2026-08-26T10:03:00Z",
  };
}

/** A running run with fixed, long-past timestamps: the server reports it stalled. */
export function inProgressRun(): Wire {
  return {
    id: "run-progress",
    ticket: "ticket-progress",
    project_path: "/projects/app",
    workspace_path: "/workspaces/run-progress",
    spec_path: "/specs/ticket-progress.md",
    spec_sha256: "spec-progress",
    state: "slice_running",
    base_sha: "base-progress",
    committed_by_factoryd: false,
    changed_files: null,
    attempts: [],
    gate_results: [],
    notifications: [],
    overrides: [],
    created_at: "2026-08-26T12:00:00Z",
    updated_at: "2026-08-26T12:01:00Z",
    stalled: true,
    stalled_since_seconds: 999999,
  };
}

/** A running run with fresh, now-relative timestamps, so "not stalled" never silently expires. */
export function waitingRun(waitingReason: string): Wire {
  const now = Date.now();
  return {
    ...inProgressRun(),
    id: "run-waiting",
    ticket: "ticket-waiting",
    stalled: false,
    stalled_since_seconds: null,
    created_at: new Date(now - 60_000).toISOString(),
    updated_at: new Date(now - 60_000).toISOString(),
    last_progress_at: new Date(now).toISOString(),
    current_stage: "build",
    waiting_reason: waitingReason,
  };
}

/** One round of BUILD_EVIDENCE.json. */
export function evidenceRound(over: Wire = {}): Wire {
  return {
    index: 1,
    agent: "pi",
    agent_returncode: 0,
    agent_timed_out: false,
    usage: { input: 20000, output: 1200 },
    verify_passed: true,
    verify_timed_out: false,
    duration_s: 41.0,
    ...over,
  };
}

export function withEvidence(run: Wire, rounds: Wire[]): Wire {
  return {
    ...run,
    agent_evidence: { schema_version: 1, succeeded: true, stopped_reason: "", rounds },
  };
}

export const deniedRelease = {
  run_id: "run-accepted",
  project: "checkouts",
  decision: {
    run_id: "run-accepted",
    project: "checkouts",
    allowed: false,
    reasons: ['kill switch is engaged for project "checkouts"'],
    evaluated_at: "2026-09-03T10:05:00Z",
  },
  kill_switch: {
    project: "checkouts",
    engaged: true,
    history: [
      {
        engaged: true,
        by: "operator@example.com",
        reason: "incident 42",
        at: "2026-09-03T09:00:00Z",
      },
    ],
  },
};

/** "reasons" is omitted and "history" is null, the server's omitempty/nil-slice shape. */
export const allowedRelease = {
  run_id: "run-accepted",
  project: "checkouts",
  decision: {
    run_id: "run-accepted",
    project: "checkouts",
    allowed: true,
    evaluated_at: "2026-09-03T10:05:00Z",
  },
  kill_switch: { project: "checkouts", engaged: false, history: null },
};

/** Nothing records a decision until a run is accepted. */
export const undecidedRelease = {
  run_id: "run-quarantined",
  project: "checkouts",
  decision: null,
  kill_switch: { project: "checkouts", engaged: false, history: null },
};

/** Evaluated but not durably recorded: distinct from `undecidedRelease`, where recording was never attempted. */
export const recordingFailedRelease = {
  run_id: "run-accepted",
  project: "checkouts",
  decision: null,
  recording_failure: {
    error: "evaluate release decision: read kill switch: unexpected end of JSON input",
    at: "2026-09-03T10:05:00Z",
    kill_switch_readable: false,
  },
  kill_switch: { project: "checkouts", engaged: false, history: null },
};
