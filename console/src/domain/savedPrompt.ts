import { type JsonObject, objectList, reqNumber, reqString } from "@/domain/decode";

/**
 * One prompt the factory sent to a coding-agent harness during a run
 * (GET /runs/{id}/prompts, internal/api.PromptView). `attempt` names the
 * launch it belongs to (build-1, code_review-1); `name` the turn.
 */
export interface SavedPrompt {
  readonly name: string;
  readonly attempt: string;
  readonly bytes: number;
  readonly time: string;
}

function decodePrompt(o: JsonObject, at: string): SavedPrompt {
  return {
    name: reqString(o, "name", at),
    attempt: reqString(o, "attempt", at),
    bytes: reqNumber(o, "bytes", at),
    time: reqString(o, "time", at),
  };
}

export function decodeSavedPrompts(o: JsonObject, at: string): SavedPrompt[] {
  return objectList(o, "prompts", at, decodePrompt);
}
