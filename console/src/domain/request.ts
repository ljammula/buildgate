// The request resource: GET /requests, GET /requests/{id}, the
// /requests/events frames and the revision routes.
import {
  type JsonObject,
  decodeList,
  isObject,
  numberOr,
  objectList,
  optBoolean,
  optNumber,
  optObject,
  optString,
  reqNumber,
  reqString,
  stringList,
  stringMap,
  stringOrNull,
} from "@/domain/decode";
import { type CostSummary, type Usage, decodeCostSummary, decodeUsage } from "@/domain/usage";

/**
 * One running drafting job (Go's request.ActiveJob): which role, model,
 * effort and route the factory is using right now.
 */
export interface ActiveJob {
  readonly stage: string;
  readonly role: string;
  readonly model: string;
  readonly modelId: string;
  readonly harness: string;
  readonly thinking: string;
  readonly route: string;
  readonly startedAt: string;
}

function decodeActiveJob(o: JsonObject, at: string): ActiveJob {
  return {
    stage: optString(o, "stage", at),
    role: optString(o, "role", at),
    model: optString(o, "model", at),
    modelId: optString(o, "model_id", at),
    harness: optString(o, "harness", at),
    thinking: optString(o, "thinking", at),
    route: optString(o, "route", at),
    startedAt: optString(o, "started_at", at),
  };
}

/**
 * "planning role · luna (gpt-5.6-luna) · harness pi · thinking max ·
 * route codex": empty parts are left out.
 */
export function activeJobLabel(job: ActiveJob): string {
  const model =
    job.model === ""
      ? job.modelId
      : job.modelId === "" || job.modelId === job.model
        ? job.model
        : `${job.model} (${job.modelId})`;
  return [
    job.role !== "" ? `${job.role} role` : "",
    model,
    job.harness !== "" ? `harness ${job.harness}` : "",
    job.thinking !== "" ? `thinking ${job.thinking}` : "",
    job.route !== "" ? `route ${job.route}` : "",
  ]
    .filter((part) => part !== "")
    .join(" · ");
}

/**
 * One ticket of a request (`internal/request.Ticket`, plus the plan file
 * `content` the server-side detail view adds for `GET /requests/{id}`:
 * empty for `GET /requests`, which does not fetch ticket file contents).
 */
export interface RequestTicket {
  readonly index: number;
  readonly specPath: string;
  readonly runId: string;
  readonly prUrl: string;
  readonly prState: string;
  readonly content: string;
  /**
   * Home-relativized (internal/api's homeRelativePath); absent on
   * GET /requests, which never fetches file contents, and on a server
   * predating this field.
   */
  readonly fullPath: string;
  /** The PR-review corrective rounds that have ended on this ticket, oldest first. */
  readonly reviewRounds: readonly ReviewRound[];
  /** The run of a corrective round under way now; "" when none is. Sent by GET /requests/{id} only. */
  readonly activeRoundRunId: string;
}

/**
 * One finished PR-review corrective round (Go's request.Round with an empty
 * kind): a build on the pull request's own branch, started by a trusted
 * reviewer's open thread.
 */
export interface ReviewRound {
  readonly index: number;
  readonly runId: string;
  /** "accepted", "quarantined" or "halted"; server-owned, so a string. */
  readonly outcome: string;
  readonly at: string;
  /** Why a round that did not end accepted failed; "" otherwise. */
  readonly error: string;
  /** An accepted round's commit reached the pull request's branch. */
  readonly pushed: boolean;
}

function decodeReviewRound(o: JsonObject, at: string): ReviewRound {
  return {
    index: numberOr(o, "index", at, 0),
    runId: optString(o, "run_id", at),
    outcome: optString(o, "outcome", at),
    at: optString(o, "at", at),
    error: optString(o, "error", at),
    pushed: optBoolean(o, "pushed", at),
  };
}

export function decodeRequestTicket(o: JsonObject, at: string): RequestTicket {
  return {
    // A round with a kind is a spec-conformity round before any pull request
    // exists; it is the build's business, not the review's.
    reviewRounds: objectList(o, "rounds", at, (round, roundAt) => ({
      kind: optString(round, "kind", roundAt),
      round: decodeReviewRound(round, roundAt),
    }))
      .filter((entry) => entry.kind === "")
      .map((entry) => entry.round),
    activeRoundRunId: optString(o, "active_round_run_id", at),
    index: reqNumber(o, "index", at),
    specPath: optString(o, "spec_path", at),
    runId: optString(o, "run_id", at),
    prUrl: optString(o, "pr_url", at),
    prState: optString(o, "pr_state", at),
    content: optString(o, "content", at),
    fullPath: optString(o, "full_path", at),
  };
}

/**
 * Token-usage evidence of the spec drafting job
 * (`internal/request.SpecEvidence`). `usage` is null when no token-usage
 * event was available: a present spec_evidence with a null usage is that
 * expected case, not a malformed response. Deliberately holds no dollar
 * figure: the agent's nested "cost" sub-object is left out of the evidence,
 * so total tokens are the console's proxy for spend.
 */
export interface SpecEvidence {
  readonly usage: Usage | null;
}

function decodeSpecEvidence(o: JsonObject, at: string): SpecEvidence {
  return { usage: optObject(o, "usage", at, decodeUsage) };
}

/** Token-usage evidence of the plan drafting job (`PlanEvidence`). */
export interface PlanEvidence {
  readonly usage: Usage | null;
}

function decodePlanEvidence(o: JsonObject, at: string): PlanEvidence {
  return { usage: optObject(o, "usage", at, decodeUsage) };
}

/**
 * One entry of RequestSummary.rejections: a structured record of a single
 * reject action, mirroring `internal/request.Rejection`.
 */
export interface Rejection {
  readonly by: string;
  readonly at: string;
  readonly reason: string;
  /**
   * The state the request was actually in when rejected (a review state, or
   * quarantined/halted for a send-back). Display and audit only: never used
   * to auto-route anything client-side, exactly like the Go field it mirrors.
   */
  readonly fromState: string;
  /**
   * Set only by a send-back: the review stage whose redraft this note feeds
   * (Go's Rejection.ForStage).
   */
  readonly forStage: string | null;
  /**
   * Notes tied to places in the reviewed files, and the free note beside
   * them. `reason` already carries all of it as text; these let a screen
   * show each note against the document. Empty for a plain rejection.
   */
  readonly anchors: readonly RejectionAnchor[];
  readonly note: string;
}

/** One note of a rejection tied to a place in a reviewed file (Go's RejectionAnchor). */
export interface RejectionAnchor {
  /** Request-relative file: "spec.md", "tickets/001.spec.md". */
  readonly path: string;
  /** The heading the note is under, as written in the file; "" for the whole file. */
  readonly section: string;
  /** The 1-based numbered item under the section; 0 for the section as a whole. */
  readonly item: number;
  readonly note: string;
}

function decodeRejectionAnchor(o: JsonObject, at: string): RejectionAnchor {
  return {
    path: reqString(o, "path", at),
    section: optString(o, "section", at),
    item: numberOr(o, "item", at, 0),
    note: reqString(o, "note", at),
  };
}

function decodeRejection(o: JsonObject, at: string): Rejection {
  return {
    by: reqString(o, "by", at),
    at: reqString(o, "at", at),
    reason: reqString(o, "reason", at),
    fromState: reqString(o, "from_state", at),
    forStage: stringOrNull(o, "for_stage", at),
    anchors: objectList(o, "anchors", at, decodeRejectionAnchor),
    note: optString(o, "note", at),
  };
}

/**
 * One in-place edit an operator saved to a reviewed file (Go's
 * request.Edit): who, when, which file, and the changed lines.
 */
export interface RequestEdit {
  readonly by: string;
  readonly at: string;
  /** Request-relative file: "spec.md", "tickets/001.spec.md". */
  readonly path: string;
  /** The review state the edit was made in. */
  readonly fromState: string;
  /** The revision holding the whole text the edit replaced. */
  readonly revision: number;
  /** Changed lines with context, each prefixed "- ", "+ " or "  ". */
  readonly diff: string;
  readonly diffTruncated: boolean;
}

function decodeRequestEdit(o: JsonObject, at: string): RequestEdit {
  return {
    by: reqString(o, "by", at),
    at: reqString(o, "at", at),
    path: reqString(o, "path", at),
    fromState: optString(o, "from_state", at),
    revision: numberOr(o, "revision", at, 0),
    diff: optString(o, "diff", at),
    diffTruncated: optBoolean(o, "diff_truncated", at),
  };
}

/**
 * The rejection that handed an edit to the drafter: the first rejection of
 * the edit's own stage made at or after it (the server quotes an edit in
 * that rejection's feedback). Null while no such rejection exists: nothing
 * has redrafted the file, so no drafter has been told.
 */
export function editHandoff(edit: RequestEdit, rejections: readonly Rejection[]): Rejection | null {
  const when = Date.parse(edit.at);
  return (
    rejections.find((r) => rejectionStage(r) === edit.fromState && Date.parse(r.at) >= when) ?? null
  );
}

/**
 * The review stage this rejection's revision belongs to (Go's
 * Rejection.Stage): forStage when a send-back set it, else fromState.
 */
export function rejectionStage(rejection: Rejection): string {
  return rejection.forStage ?? rejection.fromState;
}

/**
 * One rejected-spec/plan snapshot's metadata, mirroring
 * `internal/request.Revision`. `files` lists the snapshotted files' paths
 * relative to the request's own directory (e.g. "spec.md",
 * "tickets/001.spec.md"); the revision route reads their content back by
 * these same paths.
 */
export interface RevisionSummary {
  readonly index: number;
  readonly at: string;
  readonly by: string;
  readonly reason: string;
  readonly fromState: string;
  readonly files: readonly string[];
  /**
   * Whether the note was handed to the redraft that followed (Go's
   * Revision.FeedbackSupplied). False for a revision recorded before the
   * server kept this, which is also a redraft that never saw the note.
   */
  readonly feedbackSupplied: boolean;
  /** "" for a rejection's snapshot; "edit" for the text an operator's in-place edit replaced. */
  readonly kind: string;
}

function decodeRevisionSummary(o: JsonObject, at: string): RevisionSummary {
  return {
    index: reqNumber(o, "index", at),
    at: reqString(o, "at", at),
    by: reqString(o, "by", at),
    reason: reqString(o, "reason", at),
    fromState: reqString(o, "from_state", at),
    files: stringList(o, "files", at),
    feedbackSupplied: optBoolean(o, "feedback_supplied", at),
    kind: optString(o, "kind", at),
  };
}

/**
 * One revision's own metadata plus the content of every file it
 * snapshotted, keyed by the same relative paths RevisionSummary.files lists.
 */
export interface RevisionDetail {
  readonly index: number;
  readonly at: string;
  readonly by: string;
  readonly reason: string;
  readonly fromState: string;
  readonly files: Readonly<Record<string, string>>;
}

export function decodeRevisionDetail(o: JsonObject, at: string): RevisionDetail {
  return {
    index: reqNumber(o, "index", at),
    at: reqString(o, "at", at),
    by: reqString(o, "by", at),
    reason: reqString(o, "reason", at),
    fromState: reqString(o, "from_state", at),
    files: stringMap(o, "files", at),
  };
}

export function decodeRevisionList(value: unknown, at: string): RevisionSummary[] {
  return decodeList(value, at, decodeRevisionSummary);
}

/**
 * One entry of RequestSummary.history: a single state move this request
 * made, mirroring `internal/request.Transition`. Each pipeline step's
 * timestamp and actor come from the history entry whose `to` reached it,
 * never from `state` alone: never a bare state word with nothing under it.
 */
export interface RequestTransition {
  readonly from: string;
  readonly to: string;
  readonly at: string;
  readonly by: string;
  readonly reason: string;
}

function decodeRequestTransition(o: JsonObject, at: string): RequestTransition {
  return {
    from: reqString(o, "from", at),
    to: reqString(o, "to", at),
    at: reqString(o, "at", at),
    by: reqString(o, "by", at),
    reason: optString(o, "reason", at),
  };
}

/**
 * One criterion's eligibility verdict from `oracle_draft.criteria` (found
 * via a console operator walkthrough): parallels
 * `internal/request.OracleDraftCriterion`, one line per acceptance
 * criterion the drafter judged, whether or not it ended up covered by an
 * oracle file. Surfaced so a `none_eligible`/`drafted` outcome comes with a
 * receipt ("why wasn't criterion 3 testable") instead of only the drafter's
 * own free-text summary.
 */
export interface OracleDraftCriterion {
  readonly number: number;
  readonly eligible: boolean;
  readonly reason: string;
}

function decodeOracleDraftCriterion(o: JsonObject, at: string): OracleDraftCriterion {
  const number = optNumber(o, "number", at);
  return {
    number: number === null ? 0 : Math.trunc(number),
    eligible: optBoolean(o, "eligible", at),
    reason: optString(o, "reason", at),
  };
}

/**
 * internal/request.ResumeInfo: the step a lost worker left behind. `refused`
 * is why a resume of a lost build was refused; while it is non-empty only
 * "rebuild from scratch" or cancel are possible.
 */
export interface ResumeInfo {
  readonly fromState: string;
  readonly lostRunId: string;
  readonly generation: number;
  readonly refused: readonly string[];
}

function decodeResumeInfo(o: JsonObject, at: string): ResumeInfo {
  return {
    fromState: optString(o, "from_state", at),
    lostRunId: optString(o, "lost_run_id", at),
    generation: numberOr(o, "generation", at, 0),
    refused: stringList(o, "refused", at),
  };
}

/**
 * An operator-submitted request (`internal/request.Request`) moving through
 * the brownfield pipeline's own state machine (`submitted -> spec_drafting
 * -> spec_review -> planning -> plan_review -> building -> pr_review ->
 * done`, plus terminal `quarantined`/`halted`/`cancelled`). One type decodes
 * both `GET /requests` items and `GET /requests/{id}`; the detail-only
 * fields (spec, tickets' content, next_action, ...) take their fallback on
 * a list row. `state`, `haltKind` and a ticket's `prState` are server-owned
 * sets that can grow, so they stay strings.
 */
export interface RequestSummary {
  readonly id: string;
  readonly workspace: string;
  readonly project: string;
  readonly state: string;
  readonly oracleDraftStatus: string;
  readonly oracleDraftDetail: string;
  /**
   * Set by the server when oracle_review was approved with no oracle files
   * after a draft that was not a deliberate none_eligible.
   */
  readonly oracleSkipWarning: string;
  /** The drafting job running right now, or null. See requestRunningJob. */
  readonly activeJob: ActiveJob | null;
  readonly submittedAt: string;
  readonly updatedAt: string;
  /**
   * When this state was entered: the "waiting since" fallback until the
   * server populates waitingSince itself on entering a review state.
   */
  readonly enteredAt: string;
  /** First line of request.md, verbatim; empty on a record from before the field. */
  readonly title: string;
  readonly ticketIndex: number;
  readonly ticketCount: number;
  readonly tickets: readonly RequestTicket[];
  readonly waitingSince: string;
  readonly approvedBy: string;
  readonly approvedAt: string;
  readonly error: string;
  /**
   * The step a lost worker left behind; present once the request has entered
   * resume_review, and kept after the operator decides.
   */
  readonly resume: ResumeInfo | null;
  /**
   * Typed halt marker (internal/request.HaltKind); 'accepted_no_pr' means
   * the ticket is built and accepted and only its pull request is missing.
   */
  readonly haltKind: string;
  /** spec.md's content. Only GET /requests/{id} populates it. */
  readonly spec: string;
  /** Home-relativized; only GET /requests/{id} populates it, like `spec`. */
  readonly specFullPath: string;
  /**
   * Present on both routes: request.Request embeds the evidence directly.
   * Null until the corresponding job has completed.
   */
  readonly specEvidence: SpecEvidence | null;
  readonly planEvidence: PlanEvidence | null;
  /** Empty for a request never rejected, or one predating the field. */
  readonly rejections: readonly Rejection[];
  /** Every in-place edit an operator saved, oldest first. */
  readonly edits: readonly RequestEdit[];
  /**
   * Server-computed spend rollup. The contract fixtures carry it on both
   * routes; null when absent.
   */
  readonly costSummary: CostSummary | null;
  /** Empty for a request predating the field or never moved out of its initial state. */
  readonly history: readonly RequestTransition[];
  readonly oracleDraftCriteria: readonly OracleDraftCriterion[];
  /**
   * The single factory-authored next step. Empty on a server predating the
   * field, in which case callers fall back to their own next-step copy.
   */
  readonly nextAction: string;
  /**
   * The state Approve will move this request to, as the server's own
   * transition table computes it. Empty when the server predates the field
   * or the request is not in a state Approve accepts.
   */
  readonly approveNextState: string;
  /**
   * Submitted with `-draft-oracles`: the oracle stages are part of this
   * request's pipeline from the start.
   */
  readonly draftOracles: boolean;
  /**
   * Whether sending this request back to planning or spec drafting is legal
   * right now. Only GET /requests/{id} computes it.
   */
  readonly canSendBack: boolean;
  /**
   * canSendBack narrowed to the "plan" target (an approved spec, and no
   * oracle stage skipped): the send-back dialog defaults to "spec" and
   * disables "plan" when false.
   */
  readonly canSendBackToPlan: boolean;
  /**
   * The id of the request/run this one is queued behind; null unless
   * currently waiting on one.
   */
  readonly waitingOn: string | null;
  /**
   * Which check quarantined this request (e.g. "spec_conformity"); null
   * unless currently quarantined for that reason.
   */
  readonly quarantineCheck: string | null;
}

/**
 * oracle_draft is parsed tolerantly: anything that is not an object with
 * string fields reads as absent rather than throwing.
 */
function oracleDraftField(raw: unknown, key: string): string {
  if (!isObject(raw)) return "";
  const value = raw[key];
  return typeof value === "string" ? value : "";
}

function oracleDraftCriteria(raw: unknown, at: string): OracleDraftCriterion[] {
  const list = isObject(raw) ? raw.criteria : null;
  if (!Array.isArray(list)) return [];
  const out: OracleDraftCriterion[] = [];
  list.forEach((entry: unknown, index) => {
    if (isObject(entry)) out.push(decodeOracleDraftCriterion(entry, `${at}[${index}]`));
  });
  return out;
}

export function decodeRequestSummary(o: JsonObject, at: string): RequestSummary {
  const oracleDraft = o.oracle_draft;
  const skipWarning = o.oracle_skip_warning;
  const activeJob = o.active_job;
  return {
    id: reqString(o, "id", at),
    workspace: reqString(o, "workspace", at),
    project: reqString(o, "project", at),
    state: reqString(o, "state", at),
    submittedAt: reqString(o, "submitted_at", at),
    updatedAt: reqString(o, "updated_at", at),
    enteredAt: optString(o, "entered_at", at),
    title: optString(o, "title", at),
    ticketIndex: numberOr(o, "ticket_index", at, 0),
    ticketCount: numberOr(o, "ticket_count", at, 0),
    tickets: objectList(o, "tickets", at, decodeRequestTicket),
    waitingSince: optString(o, "waiting_since", at),
    approvedBy: optString(o, "approved_by", at),
    approvedAt: optString(o, "approved_at", at),
    error: optString(o, "error", at),
    resume: optObject(o, "resume", at, decodeResumeInfo),
    haltKind: optString(o, "halt_kind", at),
    spec: optString(o, "spec", at),
    specFullPath: optString(o, "spec_full_path", at),
    specEvidence: optObject(o, "spec_evidence", at, decodeSpecEvidence),
    planEvidence: optObject(o, "plan_evidence", at, decodePlanEvidence),
    rejections: objectList(o, "rejections", at, decodeRejection),
    edits: objectList(o, "edits", at, decodeRequestEdit),
    costSummary: optObject(o, "cost_summary", at, decodeCostSummary),
    history: objectList(o, "history", at, decodeRequestTransition),
    oracleDraftStatus: oracleDraftField(oracleDraft, "status"),
    oracleDraftDetail: oracleDraftField(oracleDraft, "detail"),
    oracleSkipWarning: typeof skipWarning === "string" ? skipWarning : "",
    activeJob: isObject(activeJob) ? decodeActiveJob(activeJob, `${at}.active_job`) : null,
    oracleDraftCriteria: oracleDraftCriteria(oracleDraft, `${at}.oracle_draft.criteria`),
    nextAction: optString(o, "next_action", at),
    canSendBack: optBoolean(o, "can_send_back", at),
    canSendBackToPlan: optBoolean(o, "can_send_back_to_plan", at),
    approveNextState: optString(o, "approve_next_state", at),
    draftOracles: optBoolean(o, "draft_oracles", at),
    waitingOn: stringOrNull(o, "waiting_on", at),
    quarantineCheck: stringOrNull(o, "quarantine_check", at),
  };
}

export function decodeRequestList(value: unknown, at: string): RequestSummary[] {
  return decodeList(value, at, decodeRequestSummary);
}

/**
 * The activeJob when it describes the stage this request is in, else null:
 * the server clears it when the job returns, but a crash can leave a stale
 * one behind, which must never read as "running".
 */
export function requestRunningJob(request: RequestSummary): ActiveJob | null {
  return request.activeJob !== null && request.activeJob.stage === request.state
    ? request.activeJob
    : null;
}

/**
 * A halted request whose only gap is a missing pull request: shown calmly
 * as accepted, not as a failure.
 */
export function requestAwaitingPullRequest(request: RequestSummary): boolean {
  return request.state === "halted" && request.haltKind === "accepted_no_pr";
}

/**
 * The calm one-line status with the exact next command (mirrors
 * internal/request.AwaitingPullRequestLabel).
 */
export function requestAwaitingPullRequestLabel(request: RequestSummary): string {
  return (
    "Accepted, awaiting pull request: the code is built and verified. " +
    `Run \`factoryd retry ${request.id}\` with pull requests enabled ` +
    "(worker -open-pull-request) to open it, or merge the branch by hand."
  );
}

/**
 * When this request started waiting on the operator: waitingSince once the
 * server sets it on entering a review state, falling back to enteredAt (this
 * state's own entry time, always set) until then. Empty only if both are
 * empty, which request.New never leaves a request in.
 */
export function requestWaitingSinceOrEnteredAt(request: RequestSummary): string {
  return request.waitingSince !== "" ? request.waitingSince : request.enteredAt;
}

/**
 * A short, board-friendly title: the title (request.md's first line
 * verbatim, backticks and all) with markdown emphasis/backtick markers
 * stripped to plain text, capped at 100 characters. Falls back to the id
 * when the title is empty.
 */
export function requestShortTitle(request: RequestSummary): string {
  const { title } = request;
  if (title === "") return request.id;
  // Found via review: a title rendered with its own backticks read as broken
  // formatting, not emphasis, as plain text.
  let stripped = title
    .replace(/^#+\s*/, "")
    .replaceAll("`", "")
    .replaceAll("**", "")
    .replaceAll("*", "")
    .trim();
  if (stripped === "") stripped = title;
  const cap = 100;
  if (stripped.length <= cap) return stripped;
  return `${stripped.substring(0, cap).trimEnd()}…`;
}
