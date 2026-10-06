import { specAcceptanceCriteria, validateTicketPlan } from "@/domain/specSkeleton";

/** One ticket of a plan, as far as coverage is concerned. */
export interface CoverageTicket {
  readonly index: number;
  readonly content: string;
}

/** One spec criterion and the tickets that claim it. */
export interface CriterionCoverage {
  /** 1-based position in the spec's list: the number a ticket claims it by. */
  readonly number: number;
  /** The criterion as the server reads it ("1. It works."). */
  readonly text: string;
  /** Indexes of the tickets claiming it, in plan order. */
  readonly tickets: readonly number[];
}

/** A ticket's claim the spec has no criterion for. */
export interface StrayClaim {
  readonly ticket: number;
  readonly numbers: readonly number[];
}

export interface PlanCoverage {
  readonly criteria: readonly CriterionCoverage[];
  /** Criteria no ticket claims: what the server's plan-coverage gate refuses. */
  readonly unclaimed: readonly number[];
  /** Claims of a number outside 1..criteria.length. The server ignores them. */
  readonly stray: readonly StrayClaim[];
  /** Tickets whose covered-criteria list does not parse: they claim nothing. */
  readonly unreadable: readonly number[];
}

/**
 * Which ticket claims which acceptance criterion (`ValidatePlanCoverage`):
 * criteria are counted from the spec's numbered list by position, a ticket
 * claims the numbers under its "### Acceptance criteria covered", and a
 * ticket that does not parse claims nothing.
 */
export function planCoverage(spec: string, tickets: readonly CoverageTicket[]): PlanCoverage {
  const texts = specAcceptanceCriteria(spec);
  const claims = tickets.map((ticket) => {
    const plan = validateTicketPlan(ticket.content);
    const readable = plan.covered.length > 0;
    return { index: ticket.index, readable, numbers: [...new Set(plan.covered)] };
  });
  const criteria = texts.map((text, i) => ({
    number: i + 1,
    text,
    tickets: claims.filter((c) => c.numbers.includes(i + 1)).map((c) => c.index),
  }));
  return {
    criteria,
    unclaimed: criteria.filter((c) => c.tickets.length === 0).map((c) => c.number),
    stray: claims
      .map((c) => ({
        ticket: c.index,
        numbers: c.numbers.filter((n) => n < 1 || n > texts.length),
      }))
      .filter((s) => s.numbers.length > 0),
    unreadable: claims.filter((c) => !c.readable).map((c) => c.index),
  };
}
