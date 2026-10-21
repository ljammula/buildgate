import { useState } from "react";

import { useRunStepLog } from "@/api/runQueries";
import type { StartupStep } from "@/domain/run";
import type { StageGlyph as Glyph } from "@/domain/runDetail";
import { escapeInvisible } from "@/domain/textEscape";
import { StageGlyph } from "@/features/run-detail/StageGlyph";
import { CodeBlock } from "@/ui/CodeBlock";
import { ErrorCallout } from "@/ui/ErrorDisplay";
import { Spinner } from "@/ui/Feedback";
import { Section } from "@/ui/PageLayout";
import { cn } from "@/ui/cn";

const labels: Readonly<Record<string, string>> = {
  image: "Sandbox image",
  workspace: "Workspace",
  install: "Install (proxy, sidecars)",
  sandbox: "Sandbox",
  verify: "Baseline verify",
  agent: "Agent",
};

const glyphs: Readonly<Record<string, Glyph>> = {
  pending: "pending",
  running: "running",
  done: "passed",
  failed: "failed",
  skipped: "skipped",
};

export interface StartupStepsProps {
  readonly runId: string;
  readonly steps: readonly StartupStep[];
  /** The run is still going, so a running step may spin. */
  readonly live: boolean;
}

function StepLog({ runId, step }: { readonly runId: string; readonly step: string }) {
  const [opened, setOpened] = useState(false);
  const log = useRunStepLog(runId, step, opened);
  return (
    <div className="flex flex-col gap-1 pl-6">
      <button
        type="button"
        className="w-fit text-xs text-fg-muted underline hover:text-fg"
        aria-expanded={opened}
        onClick={() => {
          setOpened((open) => !open);
        }}
      >
        {opened ? "Hide log" : "Open log"}
      </button>
      {opened && log.isPending ? <Spinner label="Loading the log" /> : null}
      {opened && log.error !== null ? <ErrorCallout error={log.error} /> : null}
      {opened && log.data !== undefined ? (
        <CodeBlock label={`Log of ${step}`} wrap maxHeight="max-h-72">
          {log.data}
        </CodeBlock>
      ) : null}
    </div>
  );
}

/**
 * How the run got to its first agent round, as the six steps the server
 * derives from its progress feed: image, workspace, verify, install, sandbox,
 * agent. Each shows its status, what it did, the wait it is in when it is the
 * step waiting, and a log when the factory recorded one. The detail is
 * factory-written (an image reference, the services started) but may quote an
 * error, so it renders as text with invisible characters written out. Absent
 * for a server that sends no steps.
 */
export function StartupSteps({ runId, steps, live }: StartupStepsProps) {
  if (steps.length === 0) return null;
  return (
    <Section title="Start-up" card>
      <ol className="flex flex-col" data-testid="startup-steps">
        {steps.map((step) => (
          <li
            key={step.name}
            data-testid={`startup-step-${step.name}`}
            className="flex flex-col gap-0.5 py-1"
          >
            <div className="flex items-center gap-2 text-sm">
              <StageGlyph glyph={glyphs[step.status] ?? "pending"} live={live} stalled={false} />
              <span
                className={cn(
                  step.status === "pending" || step.status === "skipped"
                    ? "text-fg-subtle"
                    : "font-medium text-fg",
                )}
              >
                {labels[step.name] ?? step.name}
              </span>
            </div>
            {step.detail !== "" ? (
              <p className="pl-6 text-xs break-words text-fg-muted">
                {escapeInvisible(step.detail)}
              </p>
            ) : null}
            {step.wait !== "" ? (
              <p className="pl-6 text-xs text-tone-warning">
                Waiting: {escapeInvisible(step.wait)}
              </p>
            ) : null}
            {step.hasLog ? <StepLog runId={runId} step={step.name} /> : null}
          </li>
        ))}
      </ol>
    </Section>
  );
}
