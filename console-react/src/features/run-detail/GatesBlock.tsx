import { CircleAlert, CircleCheck } from "lucide-react";

import type { GateResult } from "@/domain/run";
import { formatGateDuration, gatesSummary } from "@/domain/runSummary";
import { listKeys } from "@/features/run-detail/listKeys";
import { CompactId } from "@/ui/CompactId";
import { Disclosure } from "@/ui/Disclosure";
import { cn } from "@/ui/cn";

function GateRow({ gate }: { readonly gate: GateResult }) {
  const Icon = gate.passed ? CircleCheck : CircleAlert;
  return (
    <li
      data-testid={`gate-${gate.check}`}
      data-passed={gate.passed}
      className="flex flex-wrap items-center gap-x-3 gap-y-1 rounded-md border border-border bg-surface-sunken px-3 py-2 text-xs"
    >
      <Icon
        aria-hidden="true"
        className={cn("size-4 shrink-0", gate.passed ? "text-tone-success" : "text-tone-danger")}
      />
      <span className="text-sm font-semibold text-fg">{gate.check}</span>
      <span className={cn(gate.passed ? "text-fg-muted" : "font-medium text-tone-danger")}>
        {gate.passed ? "passed" : `failed · exit code ${gate.exitCode}`}
      </span>
      <span className="text-fg-muted tabular-nums">{formatGateDuration(gate.durationMs)}</span>
      <span
        title={gate.command.join(" ")}
        className="min-w-0 flex-1 truncate font-mono text-fg-muted"
      >
        {gate.command.join(" ")}
      </span>
      <span className="flex items-center gap-1 text-fg-subtle">
        Log SHA
        <CompactId value={gate.logSha256} max={14} label="log SHA-256" className="text-xs" />
      </span>
    </li>
  );
}

/** The gate results as one closed line; a failed gate opens it and is the first row. */
export function GatesBlock({ gates }: { readonly gates: readonly GateResult[] }) {
  if (gates.length === 0) return null;
  const summary = gatesSummary(gates);
  const ordered = [...gates.filter((g) => !g.passed), ...gates.filter((g) => g.passed)];
  const keys = listKeys(ordered, (gate) => gate.check);
  return (
    <Disclosure
      title="Gate results"
      summary={summary.text}
      defaultOpen={summary.failed}
      failed={summary.failed}
      testId="gates-block"
    >
      <ul className="flex flex-col gap-2">
        {ordered.map((gate, i) => (
          <GateRow key={keys[i]} gate={gate} />
        ))}
      </ul>
    </Disclosure>
  );
}
