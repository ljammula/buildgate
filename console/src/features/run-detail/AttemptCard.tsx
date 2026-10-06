import { useState } from "react";

import { formatElapsedCompact, formatLocalTimestamp, elapsedBetween } from "@/domain/elapsed";
import type { Attempt } from "@/domain/run";
import { attemptExitText, attemptModelLine } from "@/domain/runDetail";
import { attemptFailed } from "@/domain/runSummary";
import { Button } from "@/ui/Button";
import { Card, CardBody } from "@/ui/Card";
import { CompactId } from "@/ui/CompactId";
import { cn } from "@/ui/cn";

export interface AttemptCardProps {
  readonly attempt: Attempt;
  readonly onOpenLog: () => void;
}

/**
 * One attempt: its exit code and how long it took up front; the model line;
 * the raw `docker run` argv collapsed by default (it is long and rarely what
 * an operator wants first); the log path with an "Open log" action into the
 * run's build-log pane rather than plain text. A failed attempt draws its
 * exit line in the failure colour.
 */
export function AttemptCard({ attempt, onOpenLog }: AttemptCardProps) {
  const [commandOpen, setCommandOpen] = useState(false);
  const modelLine = attemptModelLine(attempt);
  const took = formatElapsedCompact(
    elapsedBetween(attempt.startedAt, attempt.finishedAt, new Date(0)),
  );
  return (
    <Card className="bg-surface-sunken">
      <CardBody className="flex flex-col gap-0.5 p-3 text-xs text-fg-muted">
        <h3 className="text-sm font-semibold text-fg">
          {attempt.kind === "" ? "Attempt" : `Attempt · ${attempt.kind}`}
        </h3>
        <p className={cn(attemptFailed(attempt) ? "font-medium text-tone-danger" : "text-fg")}>
          {attemptExitText(attempt)}
        </p>
        <p>
          <time
            dateTime={attempt.startedAt}
            title={`Finished ${formatLocalTimestamp(attempt.finishedAt)}`}
          >
            {formatLocalTimestamp(attempt.startedAt)}
          </time>
          {` · took ${took}`}
        </p>
        {modelLine !== null ? <p>{modelLine}</p> : null}
        <div className="flex items-center justify-between gap-3">
          <p className="flex min-w-0 items-center gap-1">
            Log:
            <CompactId value={attempt.logPath} max={48} label="log path" className="text-xs" />
          </p>
          <Button size="sm" variant="ghost" onClick={onOpenLog}>
            Open log
          </Button>
        </div>
        <div>
          <Button
            size="sm"
            variant="ghost"
            aria-expanded={commandOpen}
            onClick={() => {
              setCommandOpen((open) => !open);
            }}
          >
            Command
          </Button>
          {commandOpen ? (
            <p className="mt-1 font-mono text-xs break-all">{attempt.command.join(" ")}</p>
          ) : null}
        </div>
      </CardBody>
    </Card>
  );
}
