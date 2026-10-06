import type { Attempt } from "@/domain/run";
import { attemptsSummary } from "@/domain/runSummary";
import { AttemptCard } from "@/features/run-detail/AttemptCard";
import { Disclosure } from "@/ui/Disclosure";

export interface AttemptsBlockProps {
  readonly attempts: readonly Attempt[];
  readonly onOpenLog: () => void;
}

/** The run's attempts as one closed line; a failed attempt opens it. Nothing at all before the first attempt. */
export function AttemptsBlock({ attempts, onOpenLog }: AttemptsBlockProps) {
  if (attempts.length === 0) return null;
  const summary = attemptsSummary(attempts);
  return (
    <Disclosure
      title="Attempts"
      summary={summary.text}
      defaultOpen={summary.failed}
      failed={summary.failed}
      testId="attempts-block"
    >
      {attempts.map((attempt, i) => (
        <AttemptCard key={`${attempt.startedAt}-${i}`} attempt={attempt} onOpenLog={onOpenLog} />
      ))}
    </Disclosure>
  );
}
