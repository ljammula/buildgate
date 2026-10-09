import { useState } from "react";

import { useRunPromptText, useRunPrompts } from "@/api/runQueries";
import type { SavedPrompt } from "@/domain/savedPrompt";
import { CodeBlock } from "@/ui/CodeBlock";
import { Disclosure } from "@/ui/Disclosure";
import { ErrorCallout } from "@/ui/ErrorDisplay";
import { Spinner } from "@/ui/Feedback";
import { RelativeTime } from "@/ui/RelativeTime";

export interface RunPromptsCardProps {
  readonly runId: string;
}

function sizeLabel(bytes: number): string {
  return bytes < 1024 ? `${bytes} B` : `${(bytes / 1024).toFixed(1)} KiB`;
}

interface PromptRowProps {
  readonly runId: string;
  readonly prompt: SavedPrompt;
}

/** One prompt; its text is read from the server only once the operator opens it. */
function PromptRow({ runId, prompt }: PromptRowProps) {
  const [opened, setOpened] = useState(false);
  const text = useRunPromptText(runId, prompt.attempt, prompt.name, opened);
  return (
    <li data-testid="run-prompt">
      <Disclosure
        bare
        headingLevel={null}
        title={prompt.name}
        summary={
          <>
            {prompt.attempt}, {sizeLabel(prompt.bytes)}, <RelativeTime value={prompt.time} />
          </>
        }
        onOpenChange={(open) => {
          if (open) setOpened(true);
        }}
      >
        {opened && text.isPending ? <Spinner label="Loading the prompt" /> : null}
        {text.error === null ? null : <ErrorCallout error={text.error} />}
        {text.data === undefined ? null : (
          <CodeBlock label={`Prompt ${prompt.name}`} wrap maxHeight="max-h-80">
            {text.data}
          </CodeBlock>
        )}
      </Disclosure>
    </li>
  );
}

/**
 * The prompts the factory sent to a model during the run, as sent (credentials
 * redacted), each opening in a text viewer. For the operator only: they quote
 * the ticket, the record of an earlier attempt and failing output, so the text
 * is shown as text. Nothing for a run that kept none or whose list cannot be
 * read.
 */
export function RunPromptsCard({ runId }: RunPromptsCardProps) {
  const prompts = useRunPrompts(runId);
  if (prompts.data === undefined || prompts.data.length === 0) return null;
  const count = prompts.data.length;
  return (
    <div data-testid="run-prompts">
      <Disclosure
        title="Prompts sent"
        summary={`${count} ${count === 1 ? "prompt" : "prompts"} the factory sent to a model; may quote repository content`}
      >
        <ul className="flex flex-col gap-1">
          {prompts.data.map((prompt) => (
            <PromptRow key={`${prompt.attempt}/${prompt.name}`} runId={runId} prompt={prompt} />
          ))}
        </ul>
      </Disclosure>
    </div>
  );
}
