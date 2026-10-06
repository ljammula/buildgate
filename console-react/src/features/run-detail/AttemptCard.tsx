import { useState } from "react";

import { formatLocalTimestamp } from "@/domain/elapsed";
import type { Attempt } from "@/domain/run";
import { attemptExitText, attemptModelLine } from "@/domain/runDetail";
import { Button } from "@/ui/Button";
import { Card, CardBody } from "@/ui/Card";

export interface AttemptCardProps {
  readonly attempt: Attempt;
  readonly onOpenLog: () => void;
}

/**
 * One Attempts card: exit code and timing up front; the raw `docker run`
 * argv collapsed by default (it is long and rarely what an operator wants
 * first); the log path with an "Open log" action into the run's build-log
 * pane rather than plain text.
 */
export function AttemptCard({ attempt, onOpenLog }: AttemptCardProps) {
  const [commandOpen, setCommandOpen] = useState(false);
  const modelLine = attemptModelLine(attempt);
  return (
    <Card className="bg-surface-sunken">
      <CardBody className="flex flex-col gap-0.5 p-3 text-xs text-fg-muted">
        <h3 className="text-sm font-semibold text-fg">
          {attempt.kind === "" ? "Attempt" : `Attempt · ${attempt.kind}`}
        </h3>
        <p className="text-fg">{attemptExitText(attempt)}</p>
        <p>Started: {formatLocalTimestamp(attempt.startedAt)}</p>
        <p>Finished: {formatLocalTimestamp(attempt.finishedAt)}</p>
        {modelLine !== null ? <p>{modelLine}</p> : null}
        <div className="flex items-start justify-between gap-3">
          <p className="min-w-0 font-mono text-xs break-all">Log: {attempt.logPath}</p>
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
