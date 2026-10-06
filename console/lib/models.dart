import 'dart:convert';

class Attempt {
  const Attempt({
    required this.command,
    required this.startedAt,
    required this.finishedAt,
    required this.exitCode,
    required this.logPath,
    this.kind = '',
    this.role = '',
    this.harness = '',
    this.thinking = '',
    this.expectedEffort = '',
    this.relayWorkerModelId = '',
    this.relayReasoningEffort = '',
    this.relayReasoningEffortAnomaly = false,
  });

  factory Attempt.fromJson(Map<String, dynamic> json) => Attempt(
    // build, verify, full_suite_verify, review, ... -- empty on a run.json
    // predating the field.
    kind: json['kind'] as String? ?? '',
    command: _stringList(json['command']),
    startedAt: json['started_at'] as String,
    finishedAt: json['finished_at'] as String,
    exitCode: json['exit_code'] as int,
    logPath: json['log_path'] as String,
    // role/thinking (internal/run.Attempt.Role/Thinking, P0e) name the
    // session-config role ("execution"/"review") this attempt ran under
    // and the reasoning-effort level it was TOLD to use -- "" for any
    // attempt kind that never resolves a role (verify/full_suite_verify),
    // and for a run recorded before these fields existed.
    role: json['role'] as String? ?? '',
    // harness (internal/run.Attempt.Harness) is the coding-agent CLI the
    // attempt's job ran under -- "" for a run recorded before the field.
    harness: json['harness'] as String? ?? '',
    thinking: json['thinking'] as String? ?? '',
    // expectedEffort (internal/run.Attempt.ExpectedEffort) is what Pi's
    // own thinkingLevelMap translation actually turns thinking into --
    // NOT thinking itself -- is what must be compared against
    // relayReasoningEffort for a clamp hint: a model whose
    // thinkingLevelMap legitimately renames "max" to "xhigh" must not
    // read as a clamp just because thinking ("max") differs from
    // relayReasoningEffort ("xhigh") (found via review -- the false
    // positive comparing thinking directly produced).
    expectedEffort: json['expected_effort'] as String? ?? '',
    // relayReasoningEffort is what the relay actually observed on the
    // wire, which can legitimately differ from expectedEffort above (a
    // real silent clamp) -- see run.Attempt.RelayReasoningEffort's own
    // doc comment.
    relayWorkerModelId: json['relay_worker_model_id'] as String? ?? '',
    relayReasoningEffort: json['relay_reasoning_effort'] as String? ?? '',
    relayReasoningEffortAnomaly:
        json['relay_reasoning_effort_anomaly'] as bool? ?? false,
  );

  final List<String> command;
  final String startedAt;
  final String finishedAt;
  final String kind;
  final int exitCode;
  final String logPath;
  final String role;
  final String harness;
  final String thinking;
  final String expectedEffort;
  final String relayWorkerModelId;
  final String relayReasoningEffort;
  final bool relayReasoningEffortAnomaly;
}

/// One line of a run's progress feed (`GET /runs/{id}/progress`,
/// Phase 4's follow-along console) -- mirrors progress-contract.md's line
/// schema exactly. `source` is `"factory"` (a pipeline stage's own
/// start/end, stamped by factoryd) or `"worker"` (relayed from the
/// sandbox's stdout -- untrusted, display-only, never used for any gate or
/// state decision). `round`/`maxRounds` are 0 and `outcome`/`detail` are
/// `""` when not applicable, matching the server's own `omitempty` -- every
/// numeric/string field is tolerant of a missing or wrongly-typed value
/// rather than throwing, since a malformed or half-written progress line
/// must never crash this screen.
class ProgressEvent {
  const ProgressEvent({
    required this.ts,
    required this.source,
    required this.stage,
    required this.event,
    this.round = 0,
    this.maxRounds = 0,
    this.outcome = '',
    this.detail = '',
  });

  factory ProgressEvent.fromJson(Map<String, dynamic> json) => ProgressEvent(
    ts:
        DateTime.tryParse(json['ts'] as String? ?? '') ??
        DateTime.fromMillisecondsSinceEpoch(0),
    source: json['source'] as String? ?? '',
    stage: json['stage'] as String? ?? '',
    event: json['event'] as String? ?? '',
    round: (json['round'] as num?)?.toInt() ?? 0,
    maxRounds: (json['max_rounds'] as num?)?.toInt() ?? 0,
    outcome: json['outcome'] as String? ?? '',
    detail: json['detail'] as String? ?? '',
  );

  final DateTime ts;
  final String source;
  final String stage;
  final String event;
  final int round;
  final int maxRounds;
  final String outcome;
  final String detail;
}

/// What one phase launched from the target repo's compose file: the API's
/// compose_phases entry (the run's `compose/services.<phase>.json`).
class ComposePhase {
  const ComposePhase({
    required this.phase,
    required this.enabled,
    this.disabledReason = '',
    this.services = const [],
  });

  factory ComposePhase.fromJson(Map<String, dynamic> json) => ComposePhase(
    phase: json['phase'] as String? ?? '',
    enabled: json['enabled'] as bool? ?? false,
    disabledReason: json['disabled_reason'] as String? ?? '',
    services: _mapList(json['services'], ComposeService.fromJson),
  );

  final String phase;
  final bool enabled;
  final String disabledReason;
  final List<ComposeService> services;
}

/// One sidecar a phase launched: reachable by the worker at alias:port.
class ComposeService {
  const ComposeService({
    required this.name,
    required this.alias,
    required this.image,
    this.port = 0,
    this.digest = '',
  });

  factory ComposeService.fromJson(Map<String, dynamic> json) => ComposeService(
    name: json['name'] as String? ?? '',
    alias: json['alias'] as String? ?? '',
    image: json['image'] as String? ?? '',
    port: json['port'] as int? ?? 0,
    digest: json['digest'] as String? ?? '',
  );

  final String name;
  final String alias;
  final String image;
  final int port;
  final String digest;

  /// Where the worker reaches this service on the run's compose network.
  String get address => port == 0 ? alias : '$alias:$port';
}

class GateResult {
  const GateResult({
    required this.check,
    required this.command,
    required this.passed,
    required this.exitCode,
    required this.durationMs,
    required this.logSha256,
  });

  factory GateResult.fromJson(Map<String, dynamic> json) => GateResult(
    check: json['check'] as String,
    command: _stringList(json['command']),
    passed: json['passed'] as bool,
    exitCode: json['exit_code'] as int,
    durationMs: json['duration_ms'] as int,
    logSha256: json['log_sha256'] as String,
  );

  final String check;
  final List<String> command;
  final bool passed;
  final int exitCode;
  final int durationMs;
  final String logSha256;
}

class DiffStat {
  const DiffStat({
    required this.filesChanged,
    required this.insertions,
    required this.deletions,
  });

  factory DiffStat.fromJson(Map<String, dynamic> json) => DiffStat(
    filesChanged: json['files_changed'] as int,
    insertions: json['insertions'] as int,
    deletions: json['deletions'] as int,
  );

  final int filesChanged;
  final int insertions;
  final int deletions;
}

/// One ticket of a RequestSummary's own `tickets` field
/// (`internal/request.Ticket`, plus the plan file `content` the
/// server-side detail view adds for `GET /requests/{id}` — empty for
/// `GET /requests`, which does not fetch ticket file contents at all;
/// see RunApi.listRequests's own doc comment).
/// One running drafting job (Go's request.ActiveJob): which role, model,
/// effort and route the factory is using right now.
class ActiveJob {
  const ActiveJob({
    required this.stage,
    this.role = '',
    this.model = '',
    this.modelId = '',
    this.harness = '',
    this.thinking = '',
    this.route = '',
    this.startedAt = '',
  });

  factory ActiveJob.fromJson(Map<String, dynamic> json) => ActiveJob(
    stage: json['stage'] as String? ?? '',
    role: json['role'] as String? ?? '',
    model: json['model'] as String? ?? '',
    modelId: json['model_id'] as String? ?? '',
    harness: json['harness'] as String? ?? '',
    thinking: json['thinking'] as String? ?? '',
    route: json['route'] as String? ?? '',
    startedAt: json['started_at'] as String? ?? '',
  );

  final String stage;
  final String role;
  final String model;
  final String modelId;
  final String harness;
  final String thinking;
  final String route;
  final String startedAt;

  /// "planning role · luna (gpt-5.6-luna) · harness pi · thinking max ·
  /// route codex":
  /// empty parts are left out.
  String get label {
    final model = this.model.isEmpty
        ? modelId
        : (modelId.isEmpty || modelId == this.model
              ? this.model
              : '${this.model} ($modelId)');
    return [
      if (role.isNotEmpty) '$role role',
      if (model.isNotEmpty) model,
      if (harness.isNotEmpty) 'harness $harness',
      if (thinking.isNotEmpty) 'thinking $thinking',
      if (route.isNotEmpty) 'route $route',
    ].join(' · ');
  }
}

class RequestTicket {
  const RequestTicket({
    required this.index,
    this.specPath = '',
    this.runId = '',
    this.prUrl = '',
    this.prState = '',
    this.content = '',
    this.fullPath = '',
  });

  factory RequestTicket.fromJson(Map<String, dynamic> json) => RequestTicket(
    index: json['index'] as int,
    specPath: json['spec_path'] as String? ?? '',
    runId: json['run_id'] as String? ?? '',
    prUrl: json['pr_url'] as String? ?? '',
    prState: json['pr_state'] as String? ?? '',
    content: json['content'] as String? ?? '',
    // Home-relativized (see internal/api's homeRelativePath) -- absent on
    // GET /requests, which never fetches file contents (specPath's own
    // doc comment), and on a server predating this field.
    fullPath: json['full_path'] as String? ?? '',
  );

  final int index;
  final String specPath;
  final String runId;
  final String prUrl;
  final String prState;
  final String content;
  final String fullPath;
}

/// Token-usage evidence (`internal/request.SpecEvidence.Usage` /
/// `PlanEvidence.Usage`): whatever numeric fields a real `pi --print
/// --mode json` agent_end event's own per-message usage dict carried,
/// summed across every assistant message in that job's single invocation
/// (see agent/pi/scripts/build_app.py's own parse_usage doc comment for
/// the exact live-capture shape this mirrors -- `input`/`output`/
/// `cacheRead`/`cacheWrite`/`reasoning`/`totalTokens` in practice, though
/// this class does not hardcode that set since the source doc comment is
/// explicit that only "a real capture" is known, not a stable schema).
///
/// Deliberately holds no dollar figure: parse_usage's own doc comment
/// records that a nested "cost" sub-object exists in the raw wire shape
/// but is intentionally left out of what gets summed and written to
/// evidence (it was all-zero against every real, uncosted local-model
/// capture checked against). [totalTokens] is therefore this console's
/// only available proxy for "how much did this cost" -- see
/// request_cost.dart's own doc comment for how it's surfaced.
class Usage {
  const Usage({required this.fields});

  factory Usage.fromJson(Map<String, dynamic> json) => Usage(fields: json);

  final Map<String, dynamic> fields;

  /// The 'totalTokens' field when present (and not below 'output', which
  /// would make it nonsense); otherwise the sum of 'input', 'output',
  /// 'cacheRead' and 'cacheWrite' when at least one of those is numeric;
  /// otherwise null -- "no usable token figure in this usage dict" is a
  /// real, distinct case from "zero tokens used". Mirrors cmd/factoryd's
  /// roundTokens (round_summary.go), so the per-round rows and the drafting
  /// and request totals count tokens the same way (C8, operator demo,
  /// 2026-09-26). Non-numeric values are ignored: usage is agent-reported.
  int? get totalTokens {
    final total = fields['totalTokens'];
    final output = fields['output'];
    if (total is num && (output is! num || total >= output)) {
      return total.round();
    }
    final parts = [
      fields['input'],
      output,
      fields['cacheRead'],
      fields['cacheWrite'],
    ].whereType<num>();
    if (parts.isEmpty) return null;
    return parts.fold<int>(0, (sum, v) => sum + v.round());
  }
}

class SpecEvidence {
  const SpecEvidence({this.usage});

  factory SpecEvidence.fromJson(Map<String, dynamic> json) => SpecEvidence(
    // Usage "preserves JSON null when no token-usage event was
    // available" (request.go's own doc comment) -- a present
    // spec_evidence with a null usage is exactly that expected case, not
    // a malformed response.
    usage: json['usage'] == null
        ? null
        : Usage.fromJson(json['usage'] as Map<String, dynamic>),
  );

  final Usage? usage;
}

class PlanEvidence {
  const PlanEvidence({this.usage});

  factory PlanEvidence.fromJson(Map<String, dynamic> json) => PlanEvidence(
    usage: json['usage'] == null
        ? null
        : Usage.fromJson(json['usage'] as Map<String, dynamic>),
  );

  final Usage? usage;
}

/// One entry of RequestSummary.rejections: a structured record of a
/// single reject action, mirroring
/// `internal/request.Rejection`. Replaces 1.5's abandoned request.md text
/// parser (that stopgap was never implemented -- GET /requests/{id} never
/// returned request.md's content for it to parse -- see this field's own
/// doc comment on the Go side, and request_detail_screen.dart's "Audit"
/// section for how this renders).
class Rejection {
  const Rejection({
    required this.by,
    required this.at,
    required this.reason,
    required this.fromState,
    this.forStage,
  });

  factory Rejection.fromJson(Map<String, dynamic> json) => Rejection(
    by: json['by'] as String,
    at: json['at'] as String,
    reason: json['reason'] as String,
    fromState: json['from_state'] as String,
    forStage: json['for_stage'] as String?,
  );

  final String by;
  final String at;
  final String reason;
  // The state the request was actually in when rejected (a review state,
  // or quarantined/halted for a send-back) -- display/audit only.
  // Display/audit only, as a guardrail: never used to auto-route
  // anything client-side, exactly like the Go field it mirrors.
  final String fromState;
  // Set only by a send-back: the review stage whose redraft this note
  // feeds (Go's Rejection.ForStage).
  final String? forStage;

  // The review stage this rejection's revision belongs to -- Go's
  // Rejection.Stage: forStage when a send-back set it, else fromState.
  String get stage => forStage ?? fromState;
}

/// RunApi.listRevisions()'s per-entry shape: one rejected-spec/plan
/// snapshot's metadata, mirroring
/// `internal/request.Revision`. [files] lists the snapshotted files'
/// paths relative to the request's own directory (e.g. "spec.md",
/// "tickets/001.spec.md") -- RunApi.getRevision reads their content back
/// by these same paths.
class RevisionSummary {
  const RevisionSummary({
    required this.index,
    required this.at,
    required this.by,
    required this.reason,
    required this.fromState,
    required this.files,
  });

  factory RevisionSummary.fromJson(Map<String, dynamic> json) =>
      RevisionSummary(
        index: json['index'] as int,
        at: json['at'] as String,
        by: json['by'] as String,
        reason: json['reason'] as String,
        fromState: json['from_state'] as String,
        files: _stringList(json['files']),
      );

  final int index;
  final String at;
  final String by;
  final String reason;
  final String fromState;
  final List<String> files;
}

/// RunApi.getRevision()'s response shape: one revision's own metadata
/// plus the content of every file it
/// snapshotted, keyed by the same relative paths RevisionSummary.files
/// lists.
class RevisionDetail {
  const RevisionDetail({
    required this.index,
    required this.at,
    required this.by,
    required this.reason,
    required this.fromState,
    required this.files,
  });

  factory RevisionDetail.fromJson(Map<String, dynamic> json) => RevisionDetail(
    index: json['index'] as int,
    at: json['at'] as String,
    by: json['by'] as String,
    reason: json['reason'] as String,
    fromState: json['from_state'] as String,
    files: (json['files'] as Map<String, dynamic>? ?? const {}).map(
      (path, content) => MapEntry(path, content as String),
    ),
  );

  final int index;
  final String at;
  final String by;
  final String reason;
  final String fromState;
  final Map<String, String> files;
}

/// requestSummaryView's own "cost_summary" field: a best-effort dollar
/// rollup of a request's spec-drafting, plan-drafting,
/// and per-ticket build costs. Only present on GET /requests -- GET
/// /requests/{id} does not compute this (see request_cost.dart's own doc
/// comment on the console-side gap this leaves for the detail screen).
class CostSummary {
  const CostSummary({
    required this.spec,
    required this.plan,
    required this.runs,
    required this.total,
    required this.currency,
    required this.complete,
    this.subscriptionBilled = false,
    this.tokens,
    this.tokensComplete = false,
    this.byModel = const [],
    this.acceptedTickets = 0,
    this.costPerAcceptedTicketMicroUsd = 0,
  });

  factory CostSummary.fromJson(Map<String, dynamic> json) => CostSummary(
    spec: (json['spec'] as num).toDouble(),
    plan: (json['plan'] as num).toDouble(),
    runs: (json['runs'] as num).toDouble(),
    total: (json['total'] as num).toDouble(),
    currency: json['currency'] as String,
    complete: json['complete'] as bool,
    subscriptionBilled: json['subscription_billed'] as bool? ?? false,
    // tokens is absent entirely from an older server that predates this
    // field -- null (not 0) so the console can tell "no data" apart from
    // "computed, and it really is zero tokens".
    tokens: (json['tokens'] as num?)?.toInt(),
    tokensComplete: json['tokens_complete'] as bool? ?? false,
    byModel: (json['by_model'] as List<dynamic>? ?? const [])
        .map((e) => ModelUsage.fromJson(e as Map<String, dynamic>))
        .toList(),
    acceptedTickets: (json['accepted_tickets'] as num?)?.toInt() ?? 0,
    costPerAcceptedTicketMicroUsd:
        (json['cost_per_accepted_ticket_micro_usd'] as num?)?.toInt() ?? 0,
  );

  final double spec;
  final double plan;
  final double runs;
  final double total;
  final String currency;
  // False whenever any ticket's run evidence could not be read or
  // carried no cost figure -- total is then a lower bound, not an exact
  // figure (see request_cost.dart's formatCostSummary for the "≥ $x"
  // rendering this drives).
  final bool complete;
  // True when any run contributing to `runs` (a ticket's build, or a
  // corrective PR-review round) was billed to a ChatGPT/Copilot
  // subscription rather than a metered API key. `total` is still a
  // real relay-priced number either way, but at least part of it was
  // never actually charged to the operator in dollars (see
  // request_cost.dart's formatCostSummary for the label this drives).
  // Absent (omitempty) on the wire decodes as false, matching a request
  // whose runs are all metered.
  final bool subscriptionBilled;
  // Tokens is the total token count across every byModel entry -- the
  // console's headline usage figure. Null means an older server that
  // predates this field, not "zero tokens" (see request_cost.dart's
  // formatUsage).
  final int? tokens;
  // Mirrors `complete`, but judged on token availability, not dollar
  // availability -- false means `tokens` is a lower bound.
  final bool tokensComplete;
  // Per-(role, model) token+cost breakdown, sorted by role then model.
  // Empty when nothing recorded a model id yet.
  final List<ModelUsage> byModel;
  // How many of this request's tickets have a run that reached
  // run.StateAccepted -- 0 (the default) on an older server that
  // predates this field, indistinguishable from "genuinely none
  // accepted yet"; request_cost.dart only renders the per-ticket cost
  // line when this is > 0, so that ambiguity never surfaces as a
  // misleading "$0.00 per ticket".
  final int acceptedTickets;
  // total (converted to micro-USD) / acceptedTickets -- 0 when
  // acceptedTickets is 0, mirroring api.CostSummary's own contract.
  final int costPerAcceptedTicketMicroUsd;
}

/// One CostSummary.byModel entry: a per-(role, model) token+cost rollup.
/// `model` empty means the contributing evidence recorded no model id at
/// all. `role` empty means the contributing evidence recorded no
/// factory-authored role at all (an older run, or an attempt kind that
/// never resolves one -- see api's own modelUsage doc comment) -- the
/// console renders this as "unknown", never coerced to "execution".
/// `costMicroUsd` and `role` are both tolerated absent (an older server
/// that predates M3-C1): costMicroUsd defaults to 0, role to ''.
class ModelUsage {
  const ModelUsage({
    required this.model,
    required this.tokens,
    this.role = '',
    this.costMicroUsd = 0,
  });

  factory ModelUsage.fromJson(Map<String, dynamic> json) => ModelUsage(
    role: json['role'] as String? ?? '',
    model: json['model'] as String? ?? '',
    tokens: (json['tokens'] as num?)?.toInt() ?? 0,
    costMicroUsd: (json['cost_micro_usd'] as num?)?.toInt() ?? 0,
  );

  final String role;
  final String model;
  final int tokens;
  final int costMicroUsd;
}

/// One entry of RequestSummary.history: a single state move this request
/// made, mirroring `internal/request.Transition`. Powers the request
/// detail screen's "Pipeline" stepper -- each pipeline step's timestamp
/// and actor come from the history entry whose [to] reached it, never
/// from `state` alone (the operator's own ask: "never a bare state word
/// with nothing under it").
class RequestTransition {
  const RequestTransition({
    required this.from,
    required this.to,
    required this.at,
    required this.by,
    this.reason = '',
  });

  factory RequestTransition.fromJson(Map<String, dynamic> json) =>
      RequestTransition(
        from: json['from'] as String,
        to: json['to'] as String,
        at: json['at'] as String,
        by: json['by'] as String,
        reason: json['reason'] as String? ?? '',
      );

  final String from;
  final String to;
  final String at;
  final String by;
  final String reason;
}

/// One criterion's eligibility verdict from `oracle_draft.criteria`
/// (found via a console operator walkthrough): parallels
/// `internal/request.OracleDraftCriterion` -- one line per acceptance
/// criterion the drafter judged, whether or not it ended up covered by an
/// oracle file. Surfaced so a `none_eligible`/`drafted` outcome comes with
/// a receipt ("why wasn't criterion 3 testable") instead of only the
/// drafter's own free-text summary.
class OracleDraftCriterion {
  const OracleDraftCriterion({
    required this.number,
    required this.eligible,
    required this.reason,
  });

  factory OracleDraftCriterion.fromJson(Map<String, dynamic> json) =>
      OracleDraftCriterion(
        number: (json['number'] as num?)?.toInt() ?? 0,
        eligible: json['eligible'] as bool? ?? false,
        reason: json['reason'] as String? ?? '',
      );

  final int number;
  final bool eligible;
  final String reason;
}

/// One RunApi.listWorkspaces() row (internal/api's workspaceHintView): a
/// workspace POST /requests would currently accept, plus the console "New
/// request" form's own hint about what verify command it would resolve to.
class WorkspaceHint {
  const WorkspaceHint({
    required this.workspace,
    this.hasFactoryYml = false,
    this.resolvedVerifyCommand = '',
    this.verifyCommandSource = '',
  });

  factory WorkspaceHint.fromJson(Map<String, dynamic> json) => WorkspaceHint(
    workspace: json['workspace'] as String? ?? '',
    hasFactoryYml: json['has_factory_yml'] as bool? ?? false,
    resolvedVerifyCommand: json['resolved_verify_command'] as String? ?? '',
    verifyCommandSource: json['verify_command_source'] as String? ?? '',
  );

  final String workspace;
  final bool hasFactoryYml;
  final String resolvedVerifyCommand;
  final String verifyCommandSource;
}

/// internal/request.ResumeInfo: the step a lost worker left behind. [refused]
/// is why a resume of a lost build was refused; while it is non-empty only
/// "rebuild from scratch" or cancel are possible.
class ResumeInfo {
  const ResumeInfo({
    required this.fromState,
    this.lostRunId = '',
    this.generation = 0,
    this.refused = const [],
  });

  factory ResumeInfo.fromJson(Map<String, dynamic> json) => ResumeInfo(
    fromState: json['from_state'] as String? ?? '',
    lostRunId: json['lost_run_id'] as String? ?? '',
    generation: json['generation'] as int? ?? 0,
    refused: (json['refused'] as List<dynamic>?)?.cast<String>() ?? const [],
  );

  final String fromState;
  final String lostRunId;
  final int generation;
  final List<String> refused;
}

/// One entry in RunApi.listRequests()/RunApi.getRequest() — an
/// operator-submitted request (`internal/request.Request`) moving through
/// the brownfield pipeline's own state machine (`submitted ->
/// spec_drafting -> spec_review -> planning -> plan_review -> building ->
/// pr_review -> done`, plus terminal `quarantined`/`halted`/`cancelled`).
/// The console's own request board is what renders this.
class RequestSummary {
  const RequestSummary({
    required this.id,
    required this.workspace,
    required this.project,
    required this.state,
    required this.submittedAt,
    required this.updatedAt,
    this.enteredAt = '',
    this.title = '',
    this.ticketIndex = 0,
    this.ticketCount = 0,
    this.tickets = const [],
    this.waitingSince = '',
    this.approvedBy = '',
    this.approvedAt = '',
    this.error = '',
    this.haltKind = '',
    this.spec = '',
    this.specFullPath = '',
    this.specEvidence,
    this.planEvidence,
    this.rejections = const [],
    this.costSummary,
    this.history = const [],
    this.oracleDraftStatus = '',
    this.oracleDraftDetail = '',
    this.oracleSkipWarning = '',
    this.activeJob,
    this.oracleDraftCriteria = const [],
    this.nextAction = '',
    this.approveNextState = '',
    this.draftOracles = false,
    this.canSendBack = false,
    this.canSendBackToPlan = false,
    this.waitingOn,
    this.quarantineCheck,
    this.resume,
  });

  factory RequestSummary.fromJson(Map<String, dynamic> json) => RequestSummary(
    id: json['id'] as String,
    workspace: json['workspace'] as String,
    project: json['project'] as String,
    state: json['state'] as String,
    submittedAt: json['submitted_at'] as String,
    updatedAt: json['updated_at'] as String,
    // When this state was entered -- used as the "waiting since" fallback
    // (see waitingSinceOrEnteredAt below) until the server starts
    // populating waitingSince itself on entering a review state.
    enteredAt: json['entered_at'] as String? ?? '',
    // First line of request.md, added to the server's response
    // (see internal/api's requestSummaryView/requestDetailView) --
    // absent on a record from before that change, hence the fallback.
    title: json['title'] as String? ?? '',
    ticketIndex: json['ticket_index'] as int? ?? 0,
    ticketCount: json['ticket_count'] as int? ?? 0,
    tickets:
        (json['tickets'] as List<dynamic>?)
            ?.map(
              (value) => RequestTicket.fromJson(value as Map<String, dynamic>),
            )
            .toList(growable: false) ??
        const [],
    waitingSince: json['waiting_since'] as String? ?? '',
    approvedBy: json['approved_by'] as String? ?? '',
    approvedAt: json['approved_at'] as String? ?? '',
    error: json['error'] as String? ?? '',
    // The step a lost worker left behind (internal/request.ResumeInfo);
    // present once the request has entered resume_review, and kept after
    // the operator decides.
    resume: json['resume'] == null
        ? null
        : ResumeInfo.fromJson(json['resume'] as Map<String, dynamic>),
    haltKind: json['halt_kind'] as String? ?? '',
    // Only GET /requests/{id} populates this (spec.md's own content) --
    // GET /requests leaves it unset, matching that route's own choice not
    // to fetch every request's file contents just to list them.
    spec: json['spec'] as String? ?? '',
    // Home-relativized (see internal/api's homeRelativePath); only
    // GET /requests/{id} populates this, same as spec above.
    specFullPath: json['spec_full_path'] as String? ?? '',
    // Present on both GET /requests and GET /requests/{id} -- unlike
    // Spec/Tickets[].content above, request.Request embeds these two
    // fields directly (see request.go), so listRequests's plainer
    // requestSummaryView already carries them; null (via
    // `omitempty`) once the corresponding job hasn't completed yet.
    specEvidence: json['spec_evidence'] == null
        ? null
        : SpecEvidence.fromJson(json['spec_evidence'] as Map<String, dynamic>),
    planEvidence: json['plan_evidence'] == null
        ? null
        : PlanEvidence.fromJson(json['plan_evidence'] as Map<String, dynamic>),
    // Present on both GET /requests and GET /requests/{id} --
    // request.Request embeds this directly, so it survives every
    // view the same way ApprovedBy/ApprovedAt do. Empty for a request
    // never rejected, or one predating this field.
    rejections: _mapList(json['rejections'], Rejection.fromJson),
    // Only GET /requests populates this -- GET
    // /requests/{id} does not compute it (confirmed by reading
    // internal/api/server.go's requestDetailView, which has no
    // CostSummary field). See request_cost.dart's own doc comment for
    // the console-side gap this leaves on the detail screen.
    costSummary: json['cost_summary'] == null
        ? null
        : CostSummary.fromJson(json['cost_summary'] as Map<String, dynamic>),
    // Present on both GET /requests and GET /requests/{id} -- request.
    // Request embeds History directly, so it survives every view the
    // same way ApprovedBy/Rejections do. Empty for a request predating
    // this field, or one that has never moved out of its initial state.
    history: _mapList(json['history'], RequestTransition.fromJson),
    // Oracle-stage status record (`-draft-oracles` requests only); parsed
    // tolerantly -- anything that is not an object with string fields reads
    // as absent rather than throwing.
    oracleDraftStatus: _oracleDraftField(json['oracle_draft'], 'status'),
    oracleDraftDetail: _oracleDraftField(json['oracle_draft'], 'detail'),
    // Set by the server when oracle_review was approved with no oracle files
    // after a draft that was not a deliberate none_eligible.
    oracleSkipWarning: json['oracle_skip_warning'] is String
        ? json['oracle_skip_warning'] as String
        : '',
    activeJob: json['active_job'] is Map<String, dynamic>
        ? ActiveJob.fromJson(json['active_job'] as Map<String, dynamic>)
        : null,
    // Per-criterion eligibility verdicts -- tolerant of an absent
    // or malformed 'criteria' list, matching every other oracle_draft
    // sub-field's own tolerant parsing above.
    oracleDraftCriteria: () {
      final raw = json['oracle_draft'];
      final list = raw is Map ? raw['criteria'] : null;
      if (list is! List) return const <OracleDraftCriterion>[];
      return [
        for (final entry in list)
          if (entry is Map<String, dynamic>)
            OracleDraftCriterion.fromJson(entry),
      ];
    }(),
    // The single factory-authored next step (found via a console operator
    // walkthrough) -- absent on a server predating this field, in which
    // case callers fall back to their own prior hard-coded next-step copy
    // (see request_detail_screen.dart's own doc comment on this field).
    nextAction: json['next_action'] as String? ?? '',
    // Whether POST .../reject with a "to" field (send back to planning or
    // spec drafting) is legal for this request right now -- only
    // GET /requests/{id} computes this (requestDetailView), matching
    // nextAction/approveNextState's own detail-only scope; absent (false)
    // on a list-route row or a server predating this field.
    canSendBack: json['can_send_back'] as bool? ?? false,
    // canSendBack narrowed to the "plan" target (an approved spec, and no
    // oracle stage skipped) -- the send-back dialog defaults to "spec"
    // and disables "plan" when false.
    canSendBackToPlan: json['can_send_back_to_plan'] as bool? ?? false,
    // The state Approve will move this request to, as the server's own
    // transition table computes it -- replaces the Dart-side
    // nextStateAfterApprove table in approve_reject.dart wherever present.
    approveNextState: json['approve_next_state'] as String? ?? '',
    draftOracles: json['draft_oracles'] as bool? ?? false,
    // The id of the request/run running ahead of this one -- present only
    // while this request is queued behind it; absent (null) otherwise, or
    // on a server predating this field.
    waitingOn: json['waiting_on'] as String?,
    // Which check quarantined this request (e.g. "spec_conformity") --
    // present only while quarantined for that reason; absent (null)
    // otherwise, or on a server predating this field.
    quarantineCheck: json['quarantine_check'] as String?,
  );

  static String _oracleDraftField(Object? raw, String key) {
    if (raw is! Map) return '';
    final value = raw[key];
    return value is String ? value : '';
  }

  final String id;
  final String workspace;
  final String project;
  final String state;
  final String oracleDraftStatus;
  final String oracleDraftDetail;
  final String oracleSkipWarning;

  /// The drafting job running right now (see [ActiveJob]), or null.
  final ActiveJob? activeJob;

  /// [activeJob] when it describes the stage this request is in, else
  /// null: the server clears it when the job returns, but a crash can
  /// leave a stale one behind, which must never read as "running".
  ActiveJob? get runningJob =>
      activeJob != null && activeJob!.stage == state ? activeJob : null;
  final String submittedAt;
  final String updatedAt;
  final String enteredAt;
  final String title;
  final int ticketIndex;
  final int ticketCount;
  final List<RequestTicket> tickets;
  final String waitingSince;
  final String approvedBy;
  final String approvedAt;
  final String error;
  final ResumeInfo? resume;
  // Typed halt marker (internal/request.HaltKind); 'accepted_no_pr' means the
  // ticket is built and accepted and only its pull request is missing.
  final String haltKind;
  final String spec;
  final String specFullPath;
  final SpecEvidence? specEvidence;
  final PlanEvidence? planEvidence;
  final List<Rejection> rejections;
  final CostSummary? costSummary;
  final List<RequestTransition> history;
  final List<OracleDraftCriterion> oracleDraftCriteria;
  // The single factory-authored next step for this request -- see its own
  // fromJson doc comment. Empty on a server predating this field.
  final String nextAction;
  // The state Approve will move this request to -- see its own fromJson
  // doc comment. Empty when the server predates this field, or the
  // request is not currently in a state Approve accepts.
  final String approveNextState;

  /// Submitted with `-draft-oracles`: the oracle stages are part of this
  /// request's pipeline from the start.
  final bool draftOracles;

  /// Whether sending this request back to planning or spec drafting
  /// is legal right now -- see its own fromJson doc comment.
  final bool canSendBack;

  /// Whether the "plan" send-back target is legal right now -- see its
  /// own fromJson doc comment.
  final bool canSendBackToPlan;

  /// The id of the request/run this one is queued behind -- null unless
  /// currently waiting on one. See its own fromJson doc comment.
  final String? waitingOn;

  /// Which check quarantined this request (e.g. "spec_conformity") --
  /// null unless currently quarantined for that reason. See its own
  /// fromJson doc comment.
  final String? quarantineCheck;

  /// A halted request whose only gap is a missing pull request: shown calmly
  /// as accepted, not as a failure.
  bool get awaitingPullRequest =>
      state == 'halted' && haltKind == 'accepted_no_pr';

  /// The calm one-line status with the exact next command (mirrors
  /// internal/request.AwaitingPullRequestLabel).
  String get awaitingPullRequestLabel =>
      'Accepted, awaiting pull request: the code is built and verified. '
      'Run `factoryd retry $id` with pull requests enabled '
      '(worker -open-pull-request) to open it, or merge the branch by hand.';

  /// When this request started waiting on the operator: waitingSince once
  /// the server sets it on entering a review state, falling back to
  /// enteredAt (this state's own entry time, always set) until then -- see
  /// enteredAt's own doc comment. Empty only if both are empty, which
  /// request.New never leaves a request in.
  String get waitingSinceOrEnteredAt =>
      waitingSince.isNotEmpty ? waitingSince : enteredAt;

  /// A short, board/app-bar-friendly title: [title] (request.md's
  /// first line verbatim, backticks and all -- see [title]'s own fromJson
  /// doc comment) with markdown emphasis/backtick markers stripped and
  /// collapsed to plain text, capped at 100 characters. Falls back to
  /// [id] exactly like every other title-rendering call site here already
  /// does when [title] is empty.
  String get shortTitle {
    if (title.isEmpty) return id;
    // Strip the common inline-markdown markers a request.md first line
    // might carry (`code`, **bold**, *italic*, # heading marks) rather
    // than showing them verbatim -- found via review: a title rendered
    // with its own backticks read as broken formatting, not emphasis, in
    // a plain Text widget.
    var stripped = title
        .replaceAll(RegExp(r'^#+\s*'), '')
        .replaceAll('`', '')
        .replaceAll('**', '')
        .replaceAll('*', '')
        .trim();
    if (stripped.isEmpty) stripped = title;
    const cap = 100;
    if (stripped.length <= cap) return stripped;
    return '${stripped.substring(0, cap).trimRight()}…';
  }
}

/// One entry in RunApi.listProjects() — a distinct project an operator has
/// already run against, with the most recently used execution locations for
/// it, so the "New run" intake can offer a quick-fill instead of requiring
/// every field to be retyped. Derived server-side from existing durable run
/// records; there is no separate mutable "project" concept to create or
/// edit here.
class ProjectSummary {
  const ProjectSummary({
    required this.projectPath,
    required this.project,
    required this.workspacePath,
    required this.specPath,
    required this.repository,
    required this.runCount,
    required this.lastRunAt,
  });

  factory ProjectSummary.fromJson(Map<String, dynamic> json) => ProjectSummary(
    projectPath: json['project_path'] as String,
    project: json['project'] as String,
    workspacePath: json['workspace_path'] as String,
    specPath: json['spec_path'] as String,
    // Empty, not null, when the most recent run against this project never
    // used one — matches how repository is otherwise always a plain
    // (possibly empty) String across this client, e.g. NewRunScreen's own
    // controller.
    repository: json['repository'] as String? ?? '',
    runCount: json['run_count'] as int,
    lastRunAt: json['last_run_at'] as String,
  );

  final String projectPath;
  // The release-decision/kill-switch project identifier -- distinct from
  // projectPath (a filesystem path). Surfaced (found via a real GitHub
  // Codex App review of this PR) so an operator using this screen can
  // discover the exact value `factoryd kill-switch -project` expects,
  // rather than needing to inspect raw JSON to find it.
  final String project;
  final String workspacePath;
  final String specPath;
  // Found via review: without this, selecting a known project only ever
  // prefilled Workspace/Spec, still leaving Repository — required by
  // NewRunScreen whenever a run needs the shared per-repository task
  // queue — to be retyped every time despite this screen's whole purpose
  // being to avoid exactly that.
  final String repository;
  final int runCount;
  final String lastRunAt;
}

// ProjectStats mirrors internal/api's identically-shaped ProjectStats:
// GET /projects/{project}/stats's response, the per-team acceptance-rate
// figures a team lead decides whether to route real tickets here on (see
// the fable adoption review's own recommendation 4). overrideRatePercent/
// medianAcceptedCostMicroUsd are null, not 0, for a project with no
// accepted runs -- "no data" must render differently from "a real 0%/$0".
class ProjectStats {
  const ProjectStats({
    required this.project,
    required this.totalRuns,
    required this.accepted,
    required this.acceptedViaOverride,
    required this.overrideRatePercent,
    required this.quarantinedByCause,
    required this.halted,
    required this.medianAcceptedCostMicroUsd,
    this.medianAcceptedCostSubscriptionBilled = false,
    this.medianAcceptedTokens,
  });

  factory ProjectStats.fromJson(Map<String, dynamic> json) => ProjectStats(
    project: json['project'] as String,
    totalRuns: json['total_runs'] as int,
    accepted: json['accepted'] as int,
    acceptedViaOverride: json['accepted_via_override'] as int,
    overrideRatePercent: json['override_rate_percent'] as int?,
    quarantinedByCause:
        (json['quarantined_by_cause'] as Map<String, dynamic>? ?? const {}).map(
          (cause, count) => MapEntry(cause, count as int),
        ),
    halted: json['halted'] as int,
    medianAcceptedCostMicroUsd: (json['median_accepted_cost_micro_usd'] as num?)
        ?.toInt(),
    medianAcceptedCostSubscriptionBilled:
        json['median_accepted_cost_subscription_billed'] as bool? ?? false,
    medianAcceptedTokens: (json['median_accepted_tokens'] as num?)?.toInt(),
  );

  final String project;
  final int totalRuns;
  final int accepted;
  final int acceptedViaOverride;
  final int? overrideRatePercent;
  final Map<String, int> quarantinedByCause;
  final int halted;
  final int? medianAcceptedCostMicroUsd;
  // True when at least one accepted run contributing to
  // medianAcceptedCostMicroUsd was billed to a ChatGPT/Copilot
  // subscription rather than a metered API key. An "any" treatment,
  // not a per-run breakdown: this aggregate mixes runs across the whole
  // project.
  final bool medianAcceptedCostSubscriptionBilled;
  // Median total relay token count (input + output) across accepted runs,
  // computed the same way medianAcceptedCostMicroUsd is -- null for a
  // project with no accepted runs, never a misleading 0.
  final int? medianAcceptedTokens;
}

// ProjectCheckResult mirrors internal/api's identically-shaped
// ProjectCheckResult (that package's own deliberate, narrow duplication of
// cmd/factoryd's wire shape -- see its own doc comment): one
// project-bootstrap structural check's verdict, from POST
// /projects/check.
class ProjectCheckResult {
  const ProjectCheckResult({
    required this.check,
    required this.path,
    required this.passed,
    required this.reasons,
  });

  factory ProjectCheckResult.fromJson(Map<String, dynamic> json) =>
      ProjectCheckResult(
        check: json['check'] as String,
        path: json['path'] as String,
        passed: json['passed'] as bool,
        reasons: _stringList(json['reasons']),
      );

  final String check;
  final String path;
  final bool passed;
  final List<String> reasons;
}

// ProjectCheckResponse is POST /projects/check's response: the console's
// "check project setup" preview of the same project-bootstrap preflight a
// real run would otherwise fail closed on, without ever starting one.
class ProjectCheckResponse {
  const ProjectCheckResponse({required this.passed, required this.checks});

  factory ProjectCheckResponse.fromJson(Map<String, dynamic> json) =>
      ProjectCheckResponse(
        passed: json['passed'] as bool,
        checks: (json['checks'] as List<dynamic>? ?? const [])
            .map((e) => ProjectCheckResult.fromJson(e as Map<String, dynamic>))
            .toList(),
      );

  final bool passed;
  final List<ProjectCheckResult> checks;
}

class Notification {
  const Notification({
    required this.runId,
    required this.ticket,
    required this.reason,
    required this.state,
    required this.sentAt,
  });

  factory Notification.fromJson(Map<String, dynamic> json) => Notification(
    runId: json['run_id'] as String,
    ticket: json['ticket'] as String,
    reason: json['reason'] as String,
    state: json['state'] as String,
    sentAt: json['sent_at'] as String,
  );

  final String runId;
  final String ticket;
  final String reason;
  final String state;
  final String sentAt;
}

class Override {
  const Override({
    required this.by,
    required this.reason,
    required this.at,
    required this.priorState,
    required this.newState,
  });

  factory Override.fromJson(Map<String, dynamic> json) => Override(
    by: json['by'] as String,
    reason: json['reason'] as String,
    at: json['at'] as String,
    priorState: json['prior_state'] as String,
    newState: json['new_state'] as String,
  );

  final String by;
  final String reason;
  final String at;
  final String priorState;
  final String newState;
}

/// One build_app.py corrective round, from `run.AgentEvidenceRound`'s
/// BUILD_EVIDENCE.json fields. Agent-reported, informational only (see
/// AgentEvidence's own doc comment) -- every field is tolerant of a
/// missing/wrongly-typed value.
class AgentEvidenceRound {
  const AgentEvidenceRound({
    required this.index,
    required this.agentReturnCode,
    required this.agentTimedOut,
    required this.verifyPassed,
    required this.verifyTimedOut,
    required this.fastCheckRan,
    required this.fastCheckPassed,
    required this.durationS,
    required this.tokens,
  });

  factory AgentEvidenceRound.fromJson(Map<String, dynamic> json) =>
      AgentEvidenceRound(
        index: (json['index'] as num?)?.toInt() ?? 0,
        agentReturnCode: (json['agent_returncode'] as num?)?.toInt() ?? 0,
        agentTimedOut: json['agent_timed_out'] as bool? ?? false,
        verifyPassed: json['verify_passed'] as bool?,
        verifyTimedOut: json['verify_timed_out'] as bool? ?? false,
        fastCheckRan: json['fast_check_ran'] as bool? ?? false,
        fastCheckPassed: json['fast_check_passed'] as bool?,
        durationS: (json['duration_s'] as num?)?.toDouble() ?? 0,
        tokens: _roundTokens(json['usage']),
      );

  final int index;
  final int agentReturnCode;
  final bool agentTimedOut;
  final bool? verifyPassed;
  final bool verifyTimedOut;
  final bool fastCheckRan;
  final bool? fastCheckPassed;
  final double durationS;
  final int tokens;

  /// Mirrors cmd/factoryd's roundOutcome (round_summary.go) exactly: same
  /// priority order build_app.py's own round_blockers applies (timeout,
  /// then the pi invocation itself failing, then a fast check substituting
  /// for verification, then verification itself), classified from only the
  /// fields this struct actually carries.
  String get outcome {
    if (agentTimedOut || verifyTimedOut) return 'fail (timed out)';
    if (agentReturnCode != 0) return 'fail (error)';
    if (fastCheckRan && fastCheckPassed == false) return 'fail (verify)';
    if (verifyPassed != null) return verifyPassed! ? 'pass' : 'fail (verify)';
    return 'fail (error)';
  }
}

// The per-round token figure: the same definition as [Usage.totalTokens].
int _roundTokens(dynamic usage) {
  if (usage is! Map<String, dynamic>) return 0;
  return Usage(fields: usage).totalTokens ?? 0;
}

/// `run.AgentEvidence` -- best-effort, agent-reported build evidence.
/// Rendered for a human to read (the Build stage's per-round rows); never
/// consulted for any gate or state decision.
class AgentEvidence {
  const AgentEvidence({required this.rounds});

  factory AgentEvidence.fromJson(Map<String, dynamic> json) => AgentEvidence(
    rounds: _mapList(json['rounds'], AgentEvidenceRound.fromJson),
  );

  final List<AgentEvidenceRound> rounds;
}

class Run {
  const Run({
    required this.id,
    required this.ticket,
    required this.projectPath,
    required this.workspacePath,
    required this.specPath,
    required this.specSha256,
    required this.state,
    required this.haltConfirmed,
    required this.baseSha,
    required this.resultSha,
    required this.committedByFactoryd,
    required this.changedFiles,
    required this.diffStat,
    required this.diffAvailable,
    required this.diffTruncated,
    required this.attempts,
    required this.gateResults,
    required this.notifications,
    required this.overrides,
    required this.agentEvidence,
    required this.createdAt,
    required this.updatedAt,
    this.lastProgressAt,
    this.currentStage,
    this.currentRound = 0,
    this.maxRounds = 0,
    this.waitingReason,
    this.stalled = false,
    this.stalledSinceSeconds,
    this.requestId = '',
    this.temporalWorkflowId = '',
    this.byModel = const [],
    this.tokensComplete = true,
    this.referenceOracleDir = '',
    this.composePhases = const [],
  });

  factory Run.fromJson(Map<String, dynamic> json) => Run(
    id: json['id'] as String,
    ticket: json['ticket'] as String,
    projectPath: json['project_path'] as String,
    workspacePath: json['workspace_path'] as String,
    specPath: json['spec_path'] as String,
    specSha256: json['spec_sha256'] as String,
    state: json['state'] as String,
    // Absent on a run.json predating this field, same conservative
    // default as run.Run.HaltConfirmed's own JSON zero value: an old or
    // unknown record must never be mistaken for a confirmed halt.
    haltConfirmed: json['halt_confirmed'] as bool? ?? false,
    baseSha: json['base_sha'] as String,
    resultSha: json['result_sha'] as String?,
    committedByFactoryd: json['committed_by_factoryd'] as bool,
    changedFiles: json['changed_files'] == null
        ? null
        : _stringList(json['changed_files']),
    diffStat: json['diff_stat'] == null
        ? null
        : DiffStat.fromJson(json['diff_stat'] as Map<String, dynamic>),
    diffAvailable: json['diff_available'] as bool? ?? false,
    diffTruncated: json['diff_truncated'] as bool? ?? false,
    attempts: _mapList(json['attempts'], Attempt.fromJson),
    gateResults: _mapList(json['gate_results'], GateResult.fromJson),
    notifications: _mapList(json['notifications'], Notification.fromJson),
    overrides: _mapList(json['overrides'], Override.fromJson),
    agentEvidence: json['agent_evidence'] == null
        ? null
        : AgentEvidence.fromJson(
            json['agent_evidence'] as Map<String, dynamic>,
          ),
    createdAt: json['created_at'] as String,
    updatedAt: json['updated_at'] as String,
    // Server-computed progress-feed fields (see progress-contract.md's
    // "silence is a bug" addition, 2026-09-18) -- always absent/empty for
    // a terminal run, and best-effort (never fail-the-request) on the
    // server, so every one of these is tolerantly parsed as optional here
    // too.
    lastProgressAt: json['last_progress_at'] as String?,
    currentStage: json['current_stage'] as String?,
    currentRound: json['current_round'] as int? ?? 0,
    maxRounds: json['max_rounds'] as int? ?? 0,
    waitingReason: json['waiting_reason'] as String?,
    // Server-computed "silence is a bug" verdict (internal/progress.
    // Stalled) -- the single shared rule `factoryd status`/`factoryd
    // watch`/this console all now delegate to, so they can't disagree.
    // Absent/false for a terminal run or a server predating this field.
    stalled: json['stalled'] as bool? ?? false,
    stalledSinceSeconds: json['stalled_since_seconds'] as int?,
    // Links this run back to the request that started it -- empty
    // for a directly-started run (no request involved) or one predating
    // these fields.
    requestId: json['request_id'] as String? ?? '',
    temporalWorkflowId: json['temporal_workflow_id'] as String? ?? '',
    byModel: (json['by_model'] as List<dynamic>? ?? const [])
        .map((e) => ModelUsage.fromJson(e as Map<String, dynamic>))
        .toList(),
    tokensComplete: json['tokens_complete'] as bool? ?? true,
    // The -reference-oracle-dir this run was launched with (run.Run.
    // ReferenceOracleDir) -- empty for a run with no oracle stage, or one
    // predating this field. Used to hide the commit_oracles/
    // post_oracle_commit_verify timeline rows when there is no oracle
    // stage to report on (see _computeTimeline).
    referenceOracleDir: json['reference_oracle_dir'] as String? ?? '',
    // Which sidecars each phase launched (the server reads the run's
    // compose/services.<phase>.json); empty for a run without compose
    // services or a server predating this field.
    composePhases: _mapList(json['compose_phases'], ComposePhase.fromJson),
  );

  final String id;
  final String ticket;
  final String projectPath;
  final String workspacePath;
  final String specPath;
  final String specSha256;
  final String state;
  // Mirrors run.Run.HaltConfirmed — see isTerminal's own doc comment for
  // why a 'halted' state alone isn't enough to know a run is truly done.
  final bool haltConfirmed;
  final String baseSha;
  final String? resultSha;
  final bool committedByFactoryd;
  final List<String>? changedFiles;
  final DiffStat? diffStat;
  // diffAvailable mirrors run.Run.DiffAvailable — whether a diff snapshot
  // exists to fetch via RunApi.getRunDiff. False for a run predating this
  // field, or one whose evidence collection warned-and-continued on the
  // diff step; a "View diff" action should only be shown when this is
  // true, not merely when resultSha is set — found via review, a run with
  // no snapshot but a resultSha could otherwise show an actionable button
  // that only ever opens an error screen.
  final bool diffAvailable;
  final bool diffTruncated;
  final List<Attempt> attempts;
  final List<ComposePhase> composePhases;
  final List<GateResult> gateResults;
  final List<Notification> notifications;
  final List<Override> overrides;
  // Null when build_app.py predates BUILD_EVIDENCE.json, or factoryd
  // could not read/parse it -- mirrors run.Run.AgentEvidence's own nil
  // pointer meaning "not collected", distinct from evidence with no
  // rounds.
  final AgentEvidence? agentEvidence;
  final String createdAt;
  final String updatedAt;
  // The following five mirror internal/api's runView (see
  // progress-contract.md's "silence is a bug" addition, 2026-09-18):
  // computed at read time from the run's progress feed, empty/zero for a
  // terminal run since a finished run's feed is never re-read.
  final String? lastProgressAt;
  final String? currentStage;
  final int currentRound;
  final int maxRounds;
  final String? waitingReason;
  // Server-computed "silence is a bug" verdict (see [stallStatus] in
  // elapsed.dart, which is this field's only consumer) -- false/null for a
  // terminal run, since a finished run's feed is never re-read.
  final bool stalled;
  final int? stalledSinceSeconds;
  // The request this run was started for -- empty for a directly
  // started run, or a run predating this field.
  final String requestId;
  final String temporalWorkflowId;
  // Server-computed per-model token breakdown across this run's own
  // attempts (internal/api's runView.ByModel) -- the run detail screen's
  // "model id and tokens spent" line. Empty when nothing recorded a model
  // id, or on a server predating this field.
  final List<ModelUsage> byModel;

  // False when [byModel] is a lower bound (runView.TokensComplete): a
  // crash-recovered relay spend, or an evidence round with no usable token
  // figure. Absent on an older server decodes as true.
  final bool tokensComplete;

  // The -reference-oracle-dir this run was launched with -- empty for a
  // run with no oracle stage, or one predating this field.
  final String referenceOracleDir;

  // 'quarantined' is deliberately excluded — found via review: an operator
  // override (POST /runs/{id}/override) can still move a quarantined run
  // to 'accepted' or 'halted' at any later time, so a screen that loaded
  // while (or after) a run was already quarantined must keep watching for
  // that, not treat it as final. 'accepted' is unconditionally final:
  // nothing ever moves a run out of it.
  //
  // 'halted' additionally requires haltConfirmed — found via review,
  // mirroring internal/api's own terminal() fix: a run can be durably
  // recorded 'halted' before its real outcome is positively known (a
  // Temporal give-up path records a halt locally without confirmation
  // the underlying execution actually stopped, precisely so a daemon's
  // reclaim scan keeps polling for what really happened). A screen that
  // stopped watching on that unconfirmed halt would never learn its
  // later reconciliation to a different terminal state.
  bool get isTerminal =>
      state == 'accepted' || (state == 'halted' && haltConfirmed);

  /// [isTerminal] plus 'quarantined' -- for *display* purposes only (an
  /// elapsed/"last activity" calculation, never a decision to stop
  /// polling/watching). Found via a console operator walkthrough: a
  /// quarantined run's progress feed has already gone silent (nothing more
  /// will arrive), so treating it as still "in flight" for elapsed/last-
  /// activity purposes over-reported a growing stall/elapsed figure that
  /// `factoryd status` itself never showed. [isTerminal] itself stays
  /// unchanged -- see its own doc comment for why an operator override can
  /// still move a quarantined run onward, which a screen must keep
  /// watching for.
  bool get isTerminalForDisplay => isTerminal || state == 'quarantined';
}

/// RunApi.getRunDiff's response — both the diff text and whether it was cut
/// short by the server's own storage cap. Found via review: a client that
/// only kept the "diff" field silently presented a truncated diff as if it
/// were the complete change, with no way for the screen to warn an operator
/// that it wasn't.
class RunDiff {
  const RunDiff({required this.diff, required this.truncated});

  factory RunDiff.fromJson(Map<String, dynamic> json) => RunDiff(
    diff: json['diff'] as String? ?? '',
    truncated: json['truncated'] as bool? ?? false,
  );

  final String diff;
  final bool truncated;
}

/// The factory's own fail-closed release decision for one run, as recorded
/// durably when the run was accepted. Read-only evidence: nothing in this
/// system acts on an allowed decision — no merge, push, or deploy — so this
/// is an audit surface, not a pending action.
class ReleaseDecision {
  const ReleaseDecision({
    required this.runId,
    required this.project,
    required this.allowed,
    required this.reasons,
    required this.evaluatedAt,
  });

  factory ReleaseDecision.fromJson(Map<String, dynamic> json) =>
      ReleaseDecision(
        runId: json['run_id'] as String,
        project: json['project'] as String,
        allowed: json['allowed'] as bool,
        // Omitted entirely (omitempty) when an allowed decision had nothing
        // to report.
        reasons: _stringList(json['reasons']),
        evaluatedAt: json['evaluated_at'] as String,
      );

  final String runId;
  final String project;
  final bool allowed;
  final List<String> reasons;
  final String evaluatedAt;
}

/// One attributable change to a project's kill switch — who flipped it, why,
/// and when.
class KillSwitchTransition {
  const KillSwitchTransition({
    required this.engaged,
    required this.by,
    required this.reason,
    required this.at,
  });

  factory KillSwitchTransition.fromJson(Map<String, dynamic> json) =>
      KillSwitchTransition(
        engaged: json['engaged'] as bool,
        by: json['by'] as String,
        reason: json['reason'] as String,
        at: json['at'] as String,
      );

  final bool engaged;
  final String by;
  final String reason;
  final String at;
}

/// A project's durable kill-switch state plus its append-only transition
/// history. A project that has never been engaged reports engaged == false
/// with an empty history.
class KillSwitchRecord {
  const KillSwitchRecord({
    required this.project,
    required this.engaged,
    required this.history,
  });

  factory KillSwitchRecord.fromJson(Map<String, dynamic> json) =>
      KillSwitchRecord(
        project: json['project'] as String,
        engaged: json['engaged'] as bool? ?? false,
        history: _mapList(json['history'], KillSwitchTransition.fromJson),
      );

  final String project;
  final bool engaged;
  final List<KillSwitchTransition> history;
}

/// RunApi.getRunRelease's response: one run's release decision and the
/// project kill switch it was evaluated against.
class ReleaseView {
  const ReleaseView({
    required this.runId,
    required this.project,
    required this.decision,
    required this.recordingFailure,
    required this.killSwitch,
  });

  factory ReleaseView.fromJson(Map<String, dynamic> json) => ReleaseView(
    runId: json['run_id'] as String,
    project: json['project'] as String,
    // Null for a run with no decision recorded — nothing records one until
    // a run is accepted. A screen must render that as "no decision", never
    // as an allowed one.
    decision: json['decision'] == null
        ? null
        : ReleaseDecision.fromJson(json['decision'] as Map<String, dynamic>),
    // Set instead of decision when the run WAS evaluated but the decision
    // itself could not be durably recorded (e.g. an unreadable/corrupted
    // kill-switch.json) — distinct from decision == null, which means
    // recording was never attempted. See DecisionRecordingFailure's own
    // doc comment.
    recordingFailure: json['recording_failure'] == null
        ? null
        : DecisionRecordingFailure.fromJson(
            json['recording_failure'] as Map<String, dynamic>,
          ),
    killSwitch: KillSwitchRecord.fromJson(
      json['kill_switch'] as Map<String, dynamic>,
    ),
  );

  final String runId;
  final String project;
  final ReleaseDecision? decision;
  final DecisionRecordingFailure? recordingFailure;
  final KillSwitchRecord killSwitch;
}

/// The durable marker the factory leaves when it evaluated a run's release
/// decision but could not durably save it — mirrors
/// internal/release.DecisionFailure. A run in this state was evaluated,
/// unlike one with no decision recorded at all.
class DecisionRecordingFailure {
  const DecisionRecordingFailure({
    required this.error,
    required this.at,
    required this.killSwitchReadable,
  });

  factory DecisionRecordingFailure.fromJson(Map<String, dynamic> json) =>
      DecisionRecordingFailure(
        error: json['error'] as String,
        at: json['at'] as String,
        killSwitchReadable: json['kill_switch_readable'] as bool? ?? false,
      );

  final String error;
  final String at;
  final bool killSwitchReadable;
}

/// RunApi.getProjectRelease's response: one project's kill switch state and
/// history, addressed by project id alone rather than derived from a run —
/// closes the gap ReleaseView alone left: a project whose kill switch is
/// engaged but which has no runs had no console surface at all.
class ProjectReleaseView {
  const ProjectReleaseView({required this.project, required this.killSwitch});

  factory ProjectReleaseView.fromJson(Map<String, dynamic> json) =>
      ProjectReleaseView(
        project: json['project'] as String,
        killSwitch: KillSwitchRecord.fromJson(
          json['kill_switch'] as Map<String, dynamic>,
        ),
      );

  final String project;
  final KillSwitchRecord killSwitch;
}

/// GET /console-config.json's response: whether this server accepts
/// an unauthenticated write from this console's own origin, and the
/// Temporal UI base URL when configured. See RunApi.canWrite's own doc
/// comment for how [writesEnabled] combines with a build-time override
/// token. [releasePolicyWarning] is the release-policy visibility gap's
/// console half: non-null/non-empty exactly when the server's own
/// configured release policy can never allow a release decision, so the
/// board can explain why every PR is silently withheld instead of
/// leaving the operator to discover it after a first accepted run
/// produces nothing.
class ConsoleConfig {
  const ConsoleConfig({
    required this.writesEnabled,
    this.temporalUiUrl,
    this.releasePolicyWarning,
  });

  final bool writesEnabled;
  final String? temporalUiUrl;
  final String? releasePolicyWarning;
}

/// GET /daemons's per-entry shape: one companion daemon's (e.g.
/// `worker`) liveness, so the board can warn when nothing is driving
/// queued/working requests forward. Tolerant of extra/missing fields --
/// this is a best-effort operational signal, not gated on for anything.
class DaemonStatus {
  const DaemonStatus({
    required this.name,
    required this.alive,
    this.lastHeartbeatAt = '',
  });

  factory DaemonStatus.fromJson(Map<String, dynamic> json) => DaemonStatus(
    name: json['name'] as String? ?? '',
    alive: json['alive'] as bool? ?? false,
    lastHeartbeatAt: json['last_heartbeat_at'] as String? ?? '',
  );

  final String name;
  final bool alive;
  final String lastHeartbeatAt;
}

/// GET /queue-run's response: whether a live `factoryd worker` is
/// currently draining this data directory, straight from the same
/// on-disk heartbeat file GET /daemons's `worker` entry would read if
/// `factoryd serve` happened to be the one managing it. Unlike GET
/// /daemons (start-token gated, and 404s whenever the server isn't
/// managing a daemon lifecycle at all -- the common setup, where an
/// operator runs `factoryd worker` by hand), this route is read-token
/// gated like every other read route and always answers when the server
/// supports it, so the board's "worker isn't running" strip (see
/// request_list_screen.dart's own `_queueRun` field) can fire regardless
/// of how worker was started. `state` is one of "absent" (no
/// heartbeat file was ever written), "stale" (a heartbeat exists but is
/// older than the server's staleness threshold), or "alive".
class QueueRunStatus {
  const QueueRunStatus({required this.state, this.lastHeartbeat = ''});

  factory QueueRunStatus.fromJson(Map<String, dynamic> json) => QueueRunStatus(
    state: json['state'] as String? ?? 'absent',
    lastHeartbeat: json['last_heartbeat'] as String? ?? '',
  );

  final String state;
  final String lastHeartbeat;
}

List<String> _stringList(dynamic value) =>
    (value as List<dynamic>? ?? const <dynamic>[]).cast<String>();

List<T> _mapList<T>(dynamic value, T Function(Map<String, dynamic>) parse) =>
    (value as List<dynamic>? ?? const <dynamic>[])
        .map((item) => parse(item as Map<String, dynamic>))
        .toList(growable: false);

/// One file of a request's oracle/ directory, as GET /requests/{id}/oracle
/// lists it (internal/request.OracleFile).
class OracleFileInfo {
  const OracleFileInfo({
    required this.name,
    required this.size,
    required this.sha256,
  });

  factory OracleFileInfo.fromJson(Map<String, dynamic> json) => OracleFileInfo(
    name: json['name'] as String,
    size: json['size'] as int? ?? 0,
    sha256: json['sha256'] as String? ?? '',
  );

  final String name;
  final int size;
  final String sha256;
}

/// GET /requests/{id}/oracle: the oracle/ files with their hashes, every
/// problem approval would refuse, and the drafting status record.
class OracleListing {
  const OracleListing({
    required this.files,
    required this.problems,
    required this.state,
    this.draftStatus = '',
    this.draftDetail = '',
    this.proposedCommand = '',
  });

  factory OracleListing.fromJson(Map<String, dynamic> json) {
    final draft = json['oracle_draft'];
    String draftField(String key) =>
        draft is Map && draft[key] is String ? draft[key] as String : '';
    return OracleListing(
      files: _mapList(json['files'], OracleFileInfo.fromJson),
      problems: [
        for (final value in (json['problems'] as List<dynamic>? ?? const []))
          value.toString(),
      ],
      state: json['state'] as String? ?? '',
      draftStatus: draftField('status'),
      draftDetail: draftField('detail'),
      proposedCommand: draftField('proposed_command'),
    );
  }

  final List<OracleFileInfo> files;
  final List<String> problems;
  final String state;
  final String draftStatus;
  final String draftDetail;
  final String proposedCommand;
}

/// One oracle file as fetched: [text] is what the operator is shown,
/// [sha256] is the hash of the exact bytes received (never of [text],
/// which may have been decoded lossily).
class OracleFileContent {
  const OracleFileContent({required this.text, required this.sha256});

  final String text;
  final String sha256;
}

/// One MANIFEST.json entry: which criterion an oracle file covers, or why
/// none does.
class OracleManifestEntry {
  const OracleManifestEntry({
    required this.criterion,
    required this.oracleFile,
    required this.rationale,
    required this.targetPath,
    required this.supersedes,
    this.criterionIndex,
  });

  final String criterion;
  final String? oracleFile;
  final String rationale;
  final String targetPath;
  final List<String> supersedes;
  final int? criterionIndex;
}

/// Parses MANIFEST.json tolerantly: null when it is not a JSON array (the
/// raw file is still shown), and non-object elements are skipped.
List<OracleManifestEntry>? parseOracleManifest(String text) {
  final Object? decoded;
  try {
    decoded = jsonDecode(text);
  } on FormatException {
    return null;
  }
  if (decoded is! List) return null;
  String str(Object? v) => v is String ? v : '';
  return [
    for (final e in decoded)
      if (e is Map)
        OracleManifestEntry(
          criterion: str(e['criterion']),
          oracleFile: e['oracle_file'] is String
              ? e['oracle_file'] as String
              : null,
          rationale: str(e['rationale']),
          targetPath: str(e['target_path']),
          supersedes: [
            for (final v
                in (e['supersedes'] is List
                    ? e['supersedes'] as List
                    : const []))
              v.toString(),
          ],
          criterionIndex: e['criterion_index'] is int
              ? e['criterion_index'] as int
              : null,
        ),
  ];
}
