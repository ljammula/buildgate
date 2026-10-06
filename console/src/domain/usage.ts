// Token and cost figures as the API reports them. Shared by the run and
// request decoders.
import {
  type JsonObject,
  numberOr,
  objectList,
  optBoolean,
  optNumber,
  optString,
  reqBoolean,
  reqNumber,
  reqString,
} from "@/domain/decode";

/**
 * A usage dict exactly as an agent reported it (build_app.py's parse_usage
 * sums the numeric top-level fields of a capture). Its values are untyped on
 * purpose: usage is agent-reported, so anything non-numeric is ignored when
 * read, never trusted.
 */
export interface Usage {
  readonly fields: Readonly<Record<string, unknown>>;
}

export function decodeUsage(o: JsonObject): Usage {
  return { fields: o };
}

function finite(value: unknown): number | null {
  return typeof value === "number" && Number.isFinite(value) ? value : null;
}

/**
 * The `totalTokens` field when present (and not below `output`, which would
 * make it nonsense); otherwise the sum of `input`, `output`, `cacheRead` and
 * `cacheWrite` when at least one is numeric; otherwise null: "no usable
 * token figure" is distinct from "zero tokens used". Mirrors cmd/factoryd's
 * roundTokens (round_summary.go), so the per-round rows and the drafting and
 * request totals count tokens the same way.
 */
export function usageTotalTokens(usage: Usage): number | null {
  const total = finite(usage.fields.totalTokens);
  const output = finite(usage.fields.output);
  if (total !== null && (output === null || total >= output)) return Math.round(total);
  const parts = [usage.fields.input, output, usage.fields.cacheRead, usage.fields.cacheWrite]
    .map(finite)
    .filter((part) => part !== null);
  if (parts.length === 0) return null;
  return parts.reduce((sum, part) => sum + Math.round(part), 0);
}

/** One (role, model) entry of a usage breakdown: api.ModelUsage. */
export interface ModelUsage {
  /** Empty when the contributing evidence resolved no role. */
  readonly role: string;
  /** Empty when the contributing evidence recorded no model id. */
  readonly model: string;
  readonly tokens: number;
  /** 0 when no cost was recorded. */
  readonly costMicroUsd: number;
}

export function decodeModelUsage(o: JsonObject, at: string): ModelUsage {
  return {
    role: optString(o, "role", at),
    model: optString(o, "model", at),
    tokens: numberOr(o, "tokens", at, 0),
    costMicroUsd: numberOr(o, "cost_micro_usd", at, 0),
  };
}

/** A request's server-computed spend: api.CostSummary. */
export interface CostSummary {
  readonly spec: number;
  readonly plan: number;
  readonly runs: number;
  readonly total: number;
  readonly currency: string;
  /** False when some run's cost could not be read: `total` is a lower bound. */
  readonly complete: boolean;
  /**
   * True when a contributing run was billed to a ChatGPT/Copilot
   * subscription: `total` is then an API-price estimate, not a charge.
   */
  readonly subscriptionBilled: boolean;
  /** Null from a server that predates the field: "no data", not zero. */
  readonly tokens: number | null;
  /** False when `tokens` is a lower bound. */
  readonly tokensComplete: boolean;
  /** Sorted by role then model; empty when nothing recorded a model id. */
  readonly byModel: readonly ModelUsage[];
  /** 0 also from a server that predates the field. */
  readonly acceptedTickets: number;
  /** 0 when `acceptedTickets` is 0. */
  readonly costPerAcceptedTicketMicroUsd: number;
}

export function decodeCostSummary(o: JsonObject, at: string): CostSummary {
  return {
    spec: reqNumber(o, "spec", at),
    plan: reqNumber(o, "plan", at),
    runs: reqNumber(o, "runs", at),
    total: reqNumber(o, "total", at),
    currency: reqString(o, "currency", at),
    complete: reqBoolean(o, "complete", at),
    subscriptionBilled: optBoolean(o, "subscription_billed", at),
    tokens: optNumber(o, "tokens", at),
    tokensComplete: optBoolean(o, "tokens_complete", at),
    byModel: objectList(o, "by_model", at, decodeModelUsage),
    acceptedTickets: numberOr(o, "accepted_tickets", at, 0),
    costPerAcceptedTicketMicroUsd: numberOr(o, "cost_per_accepted_ticket_micro_usd", at, 0),
  };
}
