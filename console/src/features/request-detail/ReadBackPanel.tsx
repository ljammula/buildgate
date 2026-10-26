import { useEffect, useState } from "react";

import { useApi } from "@/api/ApiProvider";
import { useAnswerReadBack } from "@/api/requestQueries";
import { sha256Hex } from "@/domain/contentHash";
import type { ReadBackQuestion, RequestSummary } from "@/domain/request";
import { getOperatorName } from "@/platform/operatorIdentity";
import { Button } from "@/ui/Button";
import { ErrorCallout } from "@/ui/ErrorDisplay";
import { Field, Textarea } from "@/ui/Input";

import { Panel } from "./Panel";

/** The server's cap on one answer, in bytes (request.MaxReadBackAnswerBytes). */
export const maxReadBackAnswerBytes = 2000;

/** Whether the request shows a read-back: a drafted one, at spec_review. */
export function hasReadBack(request: RequestSummary): boolean {
  const readBack = request.readBack;
  return request.state === "spec_review" && readBack !== null && readBack.summary !== "";
}

function byteLength(text: string): number {
  return new TextEncoder().encode(text).length;
}

/**
 * A handed-over spec's read-back at spec_review: the factory's summary of
 * what it will build and the questions it would otherwise guess at, each
 * with an answer box saved on its own. Model-written text, rendered as text;
 * it decides nothing, and approval never waits on an answer.
 *
 * Each box tells `onDraftChanged` whether it holds text not yet saved, so
 * Approve can warn that the text is not sent. A box starts over from the
 * stored answer whenever that answer (or the question) changes underneath
 * it, whoever changed it.
 */
export function ReadBackPanel({
  request,
  onDraftChanged,
}: {
  readonly request: RequestSummary;
  readonly onDraftChanged: (questionId: string, unsaved: boolean) => void;
}) {
  const { canWrite } = useApi();
  if (!hasReadBack(request) || request.readBack === null) return null;
  const { summary, questions } = request.readBack;
  return (
    <Panel title="Read-back" testId="read-back">
      <p className="text-fg-muted text-sm">
        The factory's reading of your spec, written by the model. It decides nothing: approve with
        questions unanswered if they do not matter.
      </p>
      <p className="text-sm whitespace-pre-line">{summary}</p>
      {questions.length === 0 ? (
        <p className="text-fg-muted text-sm">It has no questions.</p>
      ) : (
        <>
          <p className="text-fg-muted text-sm">
            The answers you save go to every ticket's build once you approve the spec.
          </p>
          <ol className="flex flex-col gap-4">
            {questions.map((question) => (
              <li key={question.id}>
                <QuestionAnswer
                  requestId={request.id}
                  question={question}
                  canWrite={canWrite}
                  onDraftChanged={onDraftChanged}
                />
              </li>
            ))}
          </ol>
        </>
      )}
    </Panel>
  );
}

function QuestionAnswer({
  requestId,
  question,
  canWrite,
  onDraftChanged,
}: {
  readonly requestId: string;
  readonly question: ReadBackQuestion;
  readonly canWrite: boolean;
  readonly onDraftChanged: (questionId: string, unsaved: boolean) => void;
}) {
  const answer = useAnswerReadBack(requestId);
  const [draft, setDraft] = useState(question.answer);
  // The stored question and answer this box last started from: when either
  // changes (a save, or someone else's answer), the box starts over from it.
  const stored = `${question.text}\u0000${question.answer}\u0000${question.answeredAt}`;
  const [startedFrom, setStartedFrom] = useState(stored);
  if (startedFrom !== stored) {
    setStartedFrom(stored);
    setDraft(question.answer);
  }
  const tooLong = byteLength(draft.trim()) > maxReadBackAnswerBytes;
  const unchanged = draft.trim() === question.answer;
  useEffect(() => {
    onDraftChanged(question.id, !unchanged);
  }, [onDraftChanged, question.id, unchanged]);
  useEffect(
    () => () => {
      onDraftChanged(question.id, false);
    },
    [onDraftChanged, question.id],
  );
  return (
    <div className="flex flex-col gap-2" data-testid={`read-back-question-${question.id}`}>
      <Field
        label={
          <span className="whitespace-pre-line">
            {question.id}: {question.text}
          </span>
        }
        hint={
          question.answer === ""
            ? "Unanswered."
            : `Answered${question.answeredBy === "" ? "" : ` by ${question.answeredBy}`}.`
        }
        {...(tooLong ? { error: `At most ${maxReadBackAnswerBytes} bytes.` } : {})}
      >
        <Textarea
          value={draft}
          disabled={!canWrite || answer.isPending}
          aria-label={`Answer to ${question.id}`}
          onChange={(event) => {
            setDraft(event.target.value);
          }}
        />
      </Field>
      <div>
        <Button
          size="sm"
          disabled={!canWrite || answer.isPending || unchanged || tooLong}
          onClick={() => {
            answer.mutate({
              questionId: question.id,
              questionSha256: sha256Hex(question.text),
              answer: draft,
              by: getOperatorName(),
            });
          }}
        >
          {draft.trim() === "" && question.answer !== "" ? "Clear answer" : "Save answer"}
        </Button>
      </div>
      {answer.error ? <ErrorCallout error={answer.error} headingLevel={4} /> : null}
    </div>
  );
}
