import 'dart:convert';

// stalled is hardcoded true here (rather than left for the console to
// derive from created_at) because that derivation now lives entirely
// server-side (internal/progress.Stalled) -- this fixture's fixed,
// long-past created_at is exactly the case a real server would also
// report stalled for.
const inProgressRunJson = '''
{
  "id": "run-progress",
  "ticket": "ticket-progress",
  "project_path": "/projects/app",
  "workspace_path": "/workspaces/run-progress",
  "spec_path": "/specs/ticket-progress.md",
  "spec_sha256": "spec-progress",
  "state": "slice_running",
  "base_sha": "base-progress",
  "committed_by_factoryd": false,
  "changed_files": null,
  "attempts": [],
  "gate_results": [],
  "notifications": [],
  "overrides": [],
  "created_at": "2026-08-26T12:00:00Z",
  "updated_at": "2026-08-26T12:01:00Z",
  "stalled": true,
  "stalled_since_seconds": 999999
}
''';

// waitingRunJson builds a non-terminal run's JSON with the "silence is a
// bug" progress fields (progress-contract.md, 2026-09-18) set to fresh,
// now-relative timestamps -- unlike the fixed-date fixtures above, this
// one is built at test run time so "not stalled" assertions never
// silently start failing years after these fixtures were written.
String waitingRunJson({
  String id = 'run-waiting',
  String ticket = 'ticket-waiting',
  String currentStage = 'build',
  String waitingReason = '',
  Duration sinceLastProgress = Duration.zero,
}) {
  final now = DateTime.now().toUtc();
  final createdAt = now.subtract(const Duration(minutes: 1));
  final lastProgressAt = now.subtract(sinceLastProgress);
  return jsonEncode({
    'id': id,
    'ticket': ticket,
    'project_path': '/projects/app',
    'workspace_path': '/workspaces/$id',
    'spec_path': '/specs/$ticket.md',
    'spec_sha256': 'spec-$id',
    'state': 'slice_running',
    'base_sha': 'base-$id',
    'committed_by_factoryd': false,
    'changed_files': null,
    'attempts': [],
    'gate_results': [],
    'notifications': [],
    'overrides': [],
    'created_at': createdAt.toIso8601String(),
    'updated_at': createdAt.toIso8601String(),
    'last_progress_at': lastProgressAt.toIso8601String(),
    'current_stage': currentStage,
    if (waitingReason.isNotEmpty) 'waiting_reason': waitingReason,
  });
}

const acceptedRunJson = '''
{
  "id": "run-accepted",
  "ticket": "ticket-accepted",
  "project_path": "/projects/app",
  "workspace_path": "/workspaces/run-accepted",
  "spec_path": "/specs/ticket-accepted.md",
  "spec_sha256": "spec-accepted",
  "state": "accepted",
  "base_sha": "base-accepted",
  "result_sha": "result-accepted",
  "committed_by_factoryd": true,
  "changed_files": ["lib/app.dart", "test/app_test.dart"],
  "diff_stat": {"files_changed": 2, "insertions": 18, "deletions": 3},
  "diff_available": true,
  "attempts": [{
    "command": ["python3", "build_app.py", "ticket-accepted"],
    "started_at": "2026-08-26T11:00:00Z",
    "finished_at": "2026-08-26T11:02:00Z",
    "exit_code": 0,
    "log_path": "/logs/attempt.log",
    "role": "execution",
    "harness": "pifork",
    "thinking": "max",
    "expected_effort": "max",
    "relay_worker_model_id": "gpt-5.6-luna",
    "relay_reasoning_effort": "high"
  }],
  "gate_results": [{
    "check": "canonical_verify",
    "command": ["make", "verify"],
    "passed": true,
    "exit_code": 0,
    "duration_ms": 4200,
    "log_sha256": "verify-log-hash"
  }],
  "notifications": [],
  "overrides": [{
    "by": "operator@example.com",
    "reason": "Reviewed recovered evidence",
    "at": "2026-08-26T11:04:00Z",
    "prior_state": "quarantined",
    "new_state": "accepted"
  }],
  "created_at": "2026-08-26T11:00:00Z",
  "updated_at": "2026-08-26T11:04:00Z"
}
''';

const quarantinedRunJson = '''
{
  "id": "run-quarantined",
  "ticket": "ticket-quarantined",
  "project_path": "/projects/app",
  "workspace_path": "/workspaces/run-quarantined",
  "spec_path": "/specs/ticket-quarantined.md",
  "spec_sha256": "spec-quarantined",
  "state": "quarantined",
  "base_sha": "base-quarantined",
  "committed_by_factoryd": false,
  "changed_files": [],
  "attempts": [],
  "gate_results": [],
  "notifications": [{
    "run_id": "run-quarantined",
    "ticket": "ticket-quarantined",
    "reason": "canonical verification failed",
    "state": "quarantined",
    "sent_at": "2026-08-26T10:03:00Z"
  }],
  "overrides": [],
  "created_at": "2026-08-26T10:00:00Z",
  "updated_at": "2026-08-26T10:03:00Z"
}
''';

// haltedRunJson deliberately omits halt_confirmed, mirroring a
// run.json this field predates and a genuinely unconfirmed halt alike
// (both decode the same conservative way) — see Run.isTerminal's own
// doc comment for why that matters.
const haltedRunJson = '''
{
  "id": "run-halted",
  "ticket": "ticket-halted",
  "project_path": "/projects/app",
  "workspace_path": "/workspaces/run-halted",
  "spec_path": "/specs/ticket-halted.md",
  "spec_sha256": "spec-halted",
  "state": "halted",
  "base_sha": "base-halted",
  "committed_by_factoryd": false,
  "changed_files": null,
  "attempts": [],
  "gate_results": [],
  "notifications": [],
  "overrides": [],
  "created_at": "2026-08-26T09:00:00Z",
  "updated_at": "2026-08-26T09:01:00Z"
}
''';

const confirmedHaltedRunJson = '''
{
  "id": "run-halted-confirmed",
  "ticket": "ticket-halted-confirmed",
  "project_path": "/projects/app",
  "workspace_path": "/workspaces/run-halted-confirmed",
  "spec_path": "/specs/ticket-halted-confirmed.md",
  "spec_sha256": "spec-halted-confirmed",
  "state": "halted",
  "halt_confirmed": true,
  "base_sha": "base-halted-confirmed",
  "committed_by_factoryd": false,
  "changed_files": null,
  "attempts": [],
  "gate_results": [],
  "notifications": [],
  "overrides": [],
  "created_at": "2026-08-26T09:00:00Z",
  "updated_at": "2026-08-26T09:01:00Z"
}
''';

// A denied release decision for an accepted run, with the project kill
// switch engaged — the reason the decision is denied — and one attributable
// transition in its history.
const deniedReleaseJson = '''
{
  "run_id": "run-accepted",
  "project": "checkouts",
  "decision": {
    "run_id": "run-accepted",
    "project": "checkouts",
    "allowed": false,
    "reasons": ["kill switch is engaged for project \\"checkouts\\""],
    "evaluated_at": "2026-09-03T10:05:00Z"
  },
  "kill_switch": {
    "project": "checkouts",
    "engaged": true,
    "history": [{
      "engaged": true,
      "by": "operator@example.com",
      "reason": "incident 42",
      "at": "2026-09-03T09:00:00Z"
    }]
  }
}
''';

// An allowed decision for a project whose kill switch was never engaged.
// "reasons" is omitted entirely and "history" is null, matching the
// server's own omitempty/nil-slice JSON shape.
const allowedReleaseJson = '''
{
  "run_id": "run-accepted",
  "project": "checkouts",
  "decision": {
    "run_id": "run-accepted",
    "project": "checkouts",
    "allowed": true,
    "evaluated_at": "2026-09-03T10:05:00Z"
  },
  "kill_switch": {"project": "checkouts", "engaged": false, "history": null}
}
''';

// A run with no decision recorded at all — nothing records one until a run
// is accepted. Must never read as an allowed decision.
const undecidedReleaseJson = '''
{
  "run_id": "run-quarantined",
  "project": "checkouts",
  "decision": null,
  "kill_switch": {"project": "checkouts", "engaged": false, "history": null}
}
''';

// A run whose release decision was evaluated but could not be durably
// recorded (e.g. an unreadable/corrupted kill-switch.json) -- distinct from
// undecidedReleaseJson's "decision": null, which means recording was never
// attempted at all.
const recordingFailedReleaseJson = '''
{
  "run_id": "run-accepted",
  "project": "checkouts",
  "decision": null,
  "recording_failure": {
    "error": "evaluate release decision: read kill switch: unexpected end of JSON input",
    "at": "2026-09-03T10:05:00Z",
    "kill_switch_readable": false
  },
  "kill_switch": {"project": "checkouts", "engaged": false, "history": null}
}
''';

// GET /projects/{project}/release's own response shape: no run_id, no
// decision — just a project id and its kill switch, engaged with one
// attributable transition. Proves the project-level route needs no run to
// exist.
const projectReleaseJson = '''
{
  "project": "checkouts",
  "kill_switch": {
    "project": "checkouts",
    "engaged": true,
    "history": [{
      "engaged": true,
      "by": "operator@example.com",
      "reason": "incident 42",
      "at": "2026-09-03T09:00:00Z"
    }]
  }
}
''';

// requestJson builds one GET /requests(/{id})-shaped JSON object for a
// request in state, for RequestListScreen/RequestDetailScreen's own
// tests -- every state the pipeline defines
// (internal/request.State) needs a fixture that renders without error,
// so this is a builder rather than one const
// per state. Built via jsonEncode (not manual string interpolation) so a
// multi-line spec/ticket content value round-trips correctly instead of
// leaving an unescaped control character inside a hand-built JSON string.
String requestJson({
  required String id,
  required String state,
  String project = 'checkouts',
  String title = '',
  String submittedAt = '2026-09-10T09:00:00Z',
  String updatedAt = '2026-09-10T09:05:00Z',
  String enteredAt = '2026-09-10T09:05:00Z',
  String waitingSince = '',
  int ticketIndex = 0,
  int ticketCount = 0,
  String error = '',
  String spec = '',
  List<Map<String, dynamic>> tickets = const [],
  String approvedBy = '',
  String approvedAt = '',
  Map<String, dynamic>? specEvidence,
  Map<String, dynamic>? planEvidence,
  List<Map<String, dynamic>> rejections = const [],
  Map<String, dynamic>? costSummary,
  List<Map<String, dynamic>> history = const [],
  Object? oracleDraft,
  Object? oracleSkipWarning,
  String nextAction = '',
  String approveNextState = '',
  String haltKind = '',
  bool draftOracles = false,
  bool canSendBack = false,
  bool canSendBackToPlan = false,
  String? waitingOn,
  String? quarantineCheck,
  Map<String, dynamic>? resume,
}) {
  return jsonEncode({
    if (draftOracles) 'draft_oracles': true,
    if (canSendBack) 'can_send_back': true,
    if (canSendBackToPlan) 'can_send_back_to_plan': true,
    'waiting_on': ?waitingOn,
    'quarantine_check': ?quarantineCheck,
    'resume': ?resume,
    if (haltKind.isNotEmpty) 'halt_kind': haltKind,
    'oracle_draft': ?oracleDraft,
    'oracle_skip_warning': ?oracleSkipWarning,
    'id': id,
    'workspace': '/repos/$project',
    'project': project,
    'state': state,
    'title': title,
    'submitted_at': submittedAt,
    'updated_at': updatedAt,
    'entered_at': enteredAt,
    'waiting_since': waitingSince,
    'ticket_index': ticketIndex,
    'ticket_count': ticketCount,
    'error': error,
    'spec': spec,
    'tickets': tickets,
    'approved_by': approvedBy,
    'approved_at': approvedAt,
    'spec_evidence': ?specEvidence,
    'plan_evidence': ?planEvidence,
    'rejections': rejections,
    'cost_summary': ?costSummary,
    'history': history,
    'next_action': nextAction,
    'approve_next_state': approveNextState,
  });
}

// requestHistoryEntryJson builds one history[] entry for requestJson
// above -- mirrors internal/request.Transition's JSON shape.
Map<String, dynamic> requestHistoryEntryJson({
  required String from,
  required String to,
  required String at,
  required String by,
  String reason = '',
}) {
  return {'from': from, 'to': to, 'at': at, 'by': by, 'reason': reason};
}

// requestRejectionJson builds one rejections[] entry for requestJson
// above -- mirrors internal/request.Rejection's JSON shape.
Map<String, dynamic> requestRejectionJson({
  required String by,
  required String at,
  required String reason,
  required String fromState,
}) {
  return {'by': by, 'at': at, 'reason': reason, 'from_state': fromState};
}

// requestCostSummaryJson builds requestJson's own costSummary argument --
// mirrors internal/api's costSummary JSON shape.
Map<String, dynamic> requestCostSummaryJson({
  double spec = 0,
  double plan = 0,
  double runs = 0,
  double total = 0,
  String currency = 'usd',
  bool complete = true,
  int? tokens,
  bool tokensComplete = true,
  List<Map<String, dynamic>> byModel = const [],
}) {
  return {
    'spec': spec,
    'plan': plan,
    'runs': runs,
    'total': total,
    'currency': currency,
    'complete': complete,
    if (tokens != null) 'tokens': tokens,
    'tokens_complete': tokensComplete,
    if (byModel.isNotEmpty) 'by_model': byModel,
  };
}

// revisionSummaryJson builds one GET /requests/{id}/revisions entry --
// mirrors internal/request.Revision's JSON shape.
String revisionSummaryJson({
  required int index,
  required String at,
  required String by,
  required String reason,
  required String fromState,
  List<String> files = const [],
}) {
  return jsonEncode({
    'index': index,
    'at': at,
    'by': by,
    'reason': reason,
    'from_state': fromState,
    'files': files,
  });
}

// revisionDetailJson builds one GET /requests/{id}/revisions/{n}
// response -- mirrors internal/api's requestRevisionDetailView.
String revisionDetailJson({
  required int index,
  required String at,
  required String by,
  required String reason,
  required String fromState,
  Map<String, String> files = const {},
}) {
  return jsonEncode({
    'index': index,
    'at': at,
    'by': by,
    'reason': reason,
    'from_state': fromState,
    'files': files,
  });
}

// requestTicketJson builds one tickets[] entry for requestJson above.
Map<String, dynamic> requestTicketJson({
  required int index,
  String specPath = '',
  String runId = '',
  String prUrl = '',
  String prState = '',
  String content = '',
}) {
  return {
    'index': index,
    'spec_path': specPath,
    'run_id': runId,
    // A PR state implies a PR: default a URL so fixtures stay realistic
    // now that the console ignores a PR state with no PR (2026-09-24).
    'pr_url': prUrl.isEmpty && prState.isNotEmpty
        ? 'https://github.com/acme/app/pull/$index'
        : prUrl,
    'pr_state': prState,
    'content': content,
  };
}
