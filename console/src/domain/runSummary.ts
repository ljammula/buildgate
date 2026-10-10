// The run page's one-line digests: what each closed block holds and whether
// anything in it failed. A failure opens its block and sorts it first; a pass
// stays a line. Time is a parameter.
import { formatTokenCount } from "@/domain/cost";
import { elapsedBetween, formatElapsedCompact } from "@/domain/elapsed";
import type { Attempt, BaselineVerify, ComposePhase, GateResult, Run } from "@/domain/run";
import { composeServiceAddress } from "@/domain/run";
import { combinedReviewExitBase } from "@/domain/runDetail";
import { stateLabel } from "@/domain/status";

function plural(n: number, one: string, many = `${one}s`): string {
  return `${n} ${n === 1 ? one : many}`;
}

/**
 * Whether an attempt failed: a non-zero exit, except a combined review's
 * 40 + bits, where only a set bit is a failure (attemptExitText reads the
 * same way).
 */
export function attemptFailed(attempt: Pick<Attempt, "kind" | "exitCode">): boolean {
  const bits = attempt.exitCode - combinedReviewExitBase;
  if (attempt.kind === "review" && bits >= 0 && bits <= 3) return bits !== 0;
  return attempt.exitCode !== 0;
}

export interface BlockSummary {
  readonly text: string;
  readonly failed: boolean;
}

export function attemptsSummary(attempts: readonly Attempt[]): BlockSummary {
  const failed = attempts.filter(attemptFailed).length;
  if (failed > 0)
    return { text: `${plural(failed, "attempt")} failed of ${attempts.length}`, failed: true };
  return { text: plural(attempts.length, "attempt") + " · all exited 0", failed: false };
}

export function gatesSummary(gates: readonly GateResult[]): BlockSummary {
  const failed = gates.filter((g) => !g.passed);
  if (failed.length > 0) {
    return {
      text: `${failed.length} of ${plural(gates.length, "gate")} failed: ${failed.map((g) => g.check).join(", ")}`,
      failed: true,
    };
  }
  return {
    text: `${plural(gates.length, "gate")} passed: ${gates.map((g) => g.check).join(", ")}`,
    failed: false,
  };
}

/**
 * What a failed command gate's rerun on the base commit showed, as one line;
 * "" for a gate that was not rerun. `reason` and the commit are the server's
 * text and are shown as text.
 */
export function gateBaseCheckText(gate: Pick<GateResult, "baseCheck">): string {
  const check = gate.baseCheck;
  if (check === null) return "";
  const base =
    check.baseSha === "" ? "the base commit" : `the base commit ${check.baseSha.slice(0, 12)}`;
  switch (check.outcome) {
    case "fails":
      return `Also fails on ${base}: no build can fix it. Fix the gate command or the repository.`;
    case "passes":
      return `Passes on ${base}: the build's changes fail it.`;
    case "not_checked":
      return `Not checked on ${base}${check.reason === "" ? "" : `: ${check.reason}`}`;
    default:
      return `On ${base}: ${check.outcome}`;
  }
}

export function composeSummary(phases: readonly ComposePhase[]): string {
  return phases
    .map((phase) => {
      const name = phase.phase === "" ? "run" : phase.phase;
      return phase.enabled
        ? `${name}: ${phase.services.map((s) => s.name).join(", ") || "no services"}`
        : `${name}: not launched`;
    })
    .join(" · ");
}

/** One compose service as the address the worker reaches it at, for the closed row's tooltip. */
export function composeServiceLine(phase: ComposePhase): string[] {
  return phase.services.map(
    (svc) =>
      `${svc.name}: ${svc.image} at ${composeServiceAddress(svc)}${svc.digest === "" ? "" : ` (${svc.digest})`}`,
  );
}

/** Every token the run's models spent, null when nothing recorded a figure. */
function runTokens(run: Pick<Run, "byModel">): number | null {
  if (run.byModel.length === 0) return null;
  return run.byModel.reduce((sum, m) => sum + m.tokens, 0);
}

/** "478.3k tokens", with a lower-bound mark when the figure is incomplete; null with no figure. */
export function runTokensText(run: Pick<Run, "byModel" | "tokensComplete">): string | null {
  const tokens = runTokens(run);
  if (tokens === null) return null;
  return `${run.tokensComplete ? "" : "≥ "}${formatTokenCount(tokens)} tokens`;
}

/**
 * "Accepted · 1 round · 2 gates passed · 478.3k tokens · 20:00": did it pass,
 * in one line. Parts with nothing behind them (no evidence rounds, no gates,
 * no usage) are left out, never printed as "none".
 */
export function runVerdictLine(run: Run): string {
  const parts = [stateLabel(run.state)];
  if (run.agentEvidence !== null && run.agentEvidence.rounds.length > 0) {
    parts.push(plural(run.agentEvidence.rounds.length, "round"));
  }
  if (run.gateResults.length > 0) {
    const failed = run.gateResults.filter((g) => !g.passed).length;
    parts.push(
      failed === 0
        ? `${plural(run.gateResults.length, "gate")} passed`
        : `${failed} of ${plural(run.gateResults.length, "gate")} failed`,
    );
  }
  const tokens = runTokensText(run);
  if (tokens !== null) parts.push(tokens);
  parts.push(formatElapsedCompact(elapsedBetween(run.createdAt, run.updatedAt, new Date(0))));
  return parts.join(" · ");
}

/** Whether the run's verdict line should draw as a failure: anything but an accepted run. */
export function runVerdictFailed(run: Pick<Run, "state" | "gateResults">): boolean {
  return run.state !== "accepted" || run.gateResults.some((g) => !g.passed);
}

/** A gate's duration for a row: "900 ms" under a second, "4.2 s" under a minute, "2:05" past it. */
export function formatGateDuration(durationMs: number): string {
  if (durationMs < 1000) return `${Math.max(0, Math.round(durationMs))} ms`;
  if (durationMs < 60_000) return `${(durationMs / 1000).toFixed(1)} s`;
  return formatElapsedCompact(durationMs);
}

// The first name and how many others there were (run.namedAndMore).
function namedAndMore(names: readonly string[], count: number): string {
  const first = names[0];
  if (first === undefined) return `${count} tests`;
  if (count <= 1) return first;
  return `${first} and ${count - 1} more`;
}

/** The baseline verify result in one line (run.BaselineVerify.Summary). */
export function baselineVerifySummary(b: BaselineVerify): string {
  const left =
    b.leftOutOfScopeCount > 0 && (b.passed || b.expected)
      ? `the command leaves ${namedAndMore(b.leftOutOfScope, b.leftOutOfScopeCount)} outside the ticket's Allowed-Files`
      : "";
  if (b.passed) return left === "" ? "passed" : `passed, but ${left}`;
  if (left !== "") return `${baselineVerifyFailureSummary(b)}; ${left}`;
  return baselineVerifyFailureSummary(b);
}

function baselineVerifyFailureSummary(b: BaselineVerify): string {
  if (b.needsCreated !== "")
    return `failed as the ticket expects: the command needs ${b.needsCreated}, which the ticket creates`;
  if (b.failingCount === 0) {
    const s = `failed: exit ${b.exitCode}`;
    return b.firstError === "" ? s : `${s}; first error in log: ${JSON.stringify(b.firstError)}`;
  }
  const failing = namedAndMore(b.failingTests, b.failingCount);
  if (b.expected) return `failed as the ticket expects: ${failing}`;
  const s = `failed: ${failing}`;
  if (b.unnamedCount === b.failingCount && b.failingCount === 1)
    return `${s}; the ticket does not name it`;
  if (b.unnamedCount === b.failingCount) return `${s}; the ticket names none of them`;
  return `${s}; the ticket does not name ${namedAndMore(b.unnamed, b.unnamedCount)}`;
}

/** Whether the baseline result is a failure the ticket does not account for. */
export function baselineVerifyFailed(b: BaselineVerify): boolean {
  return (!b.passed && !b.expected) || b.leftOutOfScopeCount > 0;
}
