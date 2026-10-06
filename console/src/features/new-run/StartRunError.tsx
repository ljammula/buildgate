import { ApiError } from "@/domain/apiError";
import { ErrorCallout } from "@/ui/ErrorDisplay";

/**
 * A rejected start. An API error's own message parts render one bullet per
 * failing check (a rejected project-bootstrap preflight names every artifact
 * that failed; found via a real console live-validation run, 2026-09-08,
 * where it arrived as one unparsed JSON blob). A 401/403 has no per-check
 * structure and gets the start-token guidance instead, as does any
 * non-API error.
 */
export function StartRunError({ error }: { readonly error: unknown }) {
  if (error instanceof ApiError && error.status !== 401 && error.status !== 403) {
    return (
      <div role="alert" className="text-sm text-tone-danger">
        <p>Could not start run:</p>
        <ul className="list-disc pl-5">
          {error.messageParts.map((part) => (
            <li key={part}>{part}</li>
          ))}
        </ul>
      </div>
    );
  }
  // POST /runs is start-token-gated.
  return <ErrorCallout error={error} startClass />;
}
