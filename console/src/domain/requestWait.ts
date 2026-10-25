// How the console words a request's wait (the server's `wait`, Go's
// request.Wait). One sentence per kind; a kind this console does not know
// still reads as a wait, never as nothing.
import { headTruncate } from "@/domain/middleTruncate";
import type { RequestWait } from "@/domain/request";

function joinParts(parts: readonly string[]): string {
  return parts.filter((part) => part !== "").join(" · ");
}

/**
 * The wait as one line for a card or the request page: "Queued · position 2 ·
 * behind req-a" for a job slot, a named lock or slot for the rest. Null when
 * the request is not waiting. The position is the server's: nothing here
 * infers an order from timestamps.
 */
export function waitLine(wait: RequestWait | null): string | null {
  if (wait === null) return null;
  switch (wait.kind) {
    case "job_slot":
      return joinParts([
        "Queued",
        wait.position === null ? "" : `position ${wait.position}`,
        wait.on === "" ? "" : `behind ${wait.on}`,
      ]);
    case "repository":
      return joinParts(["Waiting for the repository", "another build holds it"]);
    case "repository_queue":
      return joinParts(["Waiting in the repository queue", wait.on]);
    case "model_host":
      return joinParts(["Waiting for the model host", wait.on]);
    case "compose_slot":
      return joinParts(["Waiting for a sidecar slot", wait.on]);
    case "stacked_ticket":
      return joinParts(["Waiting on the earlier ticket's branch", wait.on]);
    case "no_worker":
      return "No worker is running";
    default:
      return "Waiting";
  }
}

/** The wait as a short label for a state chip. */
export function waitChipLabel(wait: RequestWait): string {
  switch (wait.kind) {
    case "job_slot":
      return wait.on === "" ? "Queued" : `Queued behind ${headTruncate(wait.on, 12, "…")}`;
    case "repository":
      return "Waiting for repository";
    case "repository_queue":
      return "Queued on repository";
    case "model_host":
      return "Waiting for model host";
    case "compose_slot":
      return "Waiting for sidecars";
    case "stacked_ticket":
      return "Waiting on earlier ticket";
    case "no_worker":
      return "No worker";
    default:
      return "Waiting";
  }
}
