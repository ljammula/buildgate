// Test builders for the JSON GET /requests answers.
import { type RequestSummary, decodeRequestSummary } from "@/domain/request";

export interface RequestJsonOptions {
  readonly id: string;
  readonly state: string;
  readonly project?: string;
  readonly title?: string;
  readonly updatedAt?: string;
  readonly enteredAt?: string;
  readonly waitingSince?: string;
  readonly ticketIndex?: number;
  readonly ticketCount?: number;
  readonly tickets?: readonly Record<string, unknown>[];
  readonly rejections?: readonly Record<string, unknown>[];
  readonly costSummary?: Record<string, unknown>;
  readonly lastNotifiedAt?: string;
  readonly lastAsk?: string;
}

export function requestJson(o: RequestJsonOptions): Record<string, unknown> {
  const project = o.project ?? "checkouts";
  return {
    id: o.id,
    workspace: `/repos/${project}`,
    project,
    state: o.state,
    title: o.title ?? "",
    submitted_at: "2026-09-10T09:00:00Z",
    updated_at: o.updatedAt ?? "2026-09-10T09:05:00Z",
    entered_at: o.enteredAt ?? "2026-09-10T09:05:00Z",
    waiting_since: o.waitingSince ?? "",
    ticket_index: o.ticketIndex ?? 0,
    ticket_count: o.ticketCount ?? 0,
    tickets: o.tickets ?? [],
    rejections: o.rejections ?? [],
    ...(o.costSummary === undefined ? {} : { cost_summary: o.costSummary }),
    ...(o.lastNotifiedAt === undefined ? {} : { last_notified_at: o.lastNotifiedAt }),
    ...(o.lastAsk === undefined ? {} : { last_ask: o.lastAsk }),
  };
}

export function ticketJson(o: {
  readonly index: number;
  readonly runId?: string;
  readonly prState?: string;
  readonly content?: string;
}): Record<string, unknown> {
  const prState = o.prState ?? "";
  return {
    index: o.index,
    spec_path: "",
    run_id: o.runId ?? "",
    pr_url: prState === "" ? "" : `https://github.com/acme/app/pull/${o.index}`,
    pr_state: prState,
    content: o.content ?? "",
  };
}

/** A decoded summary of `requestJson(o)`, for model tests that need no HTTP. */
export function requestSummary(o: RequestJsonOptions & { readonly spec?: string }): RequestSummary {
  const { spec, ...rest } = o;
  return decodeRequestSummary(
    { ...requestJson(rest), ...(spec === undefined ? {} : { spec }) },
    "test",
  );
}
