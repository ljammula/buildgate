import { challengeVerdictLabel } from "@/domain/challenge";
import type { Run } from "@/domain/run";
import { EscapedText } from "@/shared/text/EscapedText";
import { Section } from "@/ui/PageLayout";

export interface RunChallengeCardProps {
  readonly run: Run;
}

/**
 * The challenger's result for this run (SC-022): the verdict, the commit that
 * holds the green files, and each held test file with the criterion it cites
 * and, for a red one, the tests that failed. Paths, assertions and notes are
 * agent-written, so they are rendered as text. Nothing for a run with no
 * challenge.
 */
export function RunChallengeCard({ run }: RunChallengeCardProps) {
  const challenge = run.challenge;
  if (challenge === null) return null;
  return (
    <div data-testid="run-challenge">
      <Section title="Challenge" card>
        <div className="flex flex-col gap-3">
          <p className="flex flex-wrap items-baseline gap-x-3 text-sm">
            <span className="font-medium text-fg">{`Verdict: ${challengeVerdictLabel(challenge.verdict)}`}</span>
            {challenge.rerun ? (
              <span className="text-xs text-fg-muted">
                Ran the held files of an earlier challenge
              </span>
            ) : null}
            {challenge.verdict === "green" && challenge.committedSha !== "" ? (
              <span className="font-mono text-xs text-fg-muted">
                {`Committed ${challenge.committedSha.slice(0, 12)}`}
              </span>
            ) : null}
          </p>
          {challenge.verdict === "no_verdict" && challenge.cause !== "" ? (
            <p className="text-sm text-fg-muted">
              <EscapedText text={`Cause: ${challenge.cause}`} />
            </p>
          ) : null}
          {challenge.files.length === 0 ? null : (
            <ul className="flex flex-col">
              {challenge.files.map((file) => (
                <li
                  key={file.path}
                  data-testid="run-challenge-file"
                  className="flex flex-col gap-0.5 border-b border-border py-2 last:border-b-0"
                >
                  <p className="flex flex-wrap items-baseline gap-x-3 text-sm">
                    <span className="font-mono font-medium text-fg">
                      <EscapedText text={file.path} />
                    </span>
                    {file.criterion === "" ? null : (
                      <span className="text-xs text-fg-muted">
                        <EscapedText text={file.criterion} />
                      </span>
                    )}
                    <span className="text-xs text-fg-muted">
                      {challengeVerdictLabel(file.verdict)}
                    </span>
                  </p>
                  {file.refused === "" ? null : (
                    <p className="text-xs text-fg-muted">
                      <EscapedText text={`Refused: ${file.refused}`} />
                    </p>
                  )}
                  {file.notRun === "" ? null : (
                    <p className="text-xs text-fg-muted">
                      <EscapedText text={`Not run: ${file.notRun}`} />
                    </p>
                  )}
                  {file.failures.length === 0 ? null : (
                    <ul className="flex flex-col gap-0.5">
                      {file.failures.map((failure, i) => (
                        <li key={i} className="font-mono text-xs break-words whitespace-pre-wrap">
                          <EscapedText
                            text={
                              failure.assertion === ""
                                ? failure.test
                                : `${failure.test}: ${failure.assertion}`
                            }
                          />
                        </li>
                      ))}
                    </ul>
                  )}
                </li>
              ))}
            </ul>
          )}
          {challenge.notes.length === 0 ? null : (
            <ul className="flex flex-col gap-0.5 text-xs text-fg-muted">
              {challenge.notes.map((note, i) => (
                <li key={i}>
                  <EscapedText text={note} />
                </li>
              ))}
            </ul>
          )}
        </div>
      </Section>
    </div>
  );
}
