import { useState } from "react";

import { ApiError } from "@/domain/apiError";
import { Button } from "@/ui/Button";

/**
 * An operator-facing classification of a caught error: a short `headline`
 * plus a concrete `nextStep`, with the original text kept in `raw` for the
 * collapsed "Details" section. Nothing is hidden, only demoted below the
 * actionable summary.
 */
export interface ErrorSummary {
  readonly headline: string;
  readonly nextStep: string;
  readonly raw: string;
}

const CONNECTION_MARKERS = [
  "connection refused",
  "failed host lookup",
  "socketexception",
  "failed to fetch",
  "clientexception",
  "networkerror when attempting to fetch",
  "load failed",
];

function rawText(error: unknown): string {
  if (error instanceof Error) {
    return error.name === "Error" ? error.message : `${error.name}: ${error.message}`;
  }
  return String(error);
}

/**
 * Classifies `error`. An ApiError carries a real HTTP status, so it is
 * checked first and takes priority over the connection-level text match.
 *
 * `startClass` marks a call to a start-token-gated route (starting a run,
 * the daemon lifecycle routes, the release and stats views), so a 401/403
 * can name the one concrete fix (open serve's own printed console link)
 * instead of the generic three-token guidance, which is still used for a
 * plain read or override 401/403 since the server's error body cannot
 * disambiguate those.
 */
export function describeError(error: unknown, { startClass = false } = {}): ErrorSummary {
  if (error instanceof ApiError) {
    const raw = error.messageParts.join("\n");
    if (error.status === 401 || error.status === 403) {
      if (startClass) {
        return {
          headline: "Not authorized",
          nextStep:
            "This browser's start token is missing or is from a " +
            "previous `factoryd serve` run -- the token changes every " +
            "restart. Open the console link `factoryd serve` printed in " +
            "its own log/terminal output just now (ends in `#t=...`), " +
            "which reloads this page with the current token.",
          raw,
        };
      }
      return {
        headline: "Not authorized",
        nextStep:
          "This request's token doesn't match what factoryd expects " +
          "(FACTORYD_API_READ_TOKEN for run/request reads, " +
          "FACTORYD_API_START_TOKEN for starting runs and the " +
          "release/stats/daemon views, or FACTORYD_API_OVERRIDE_TOKEN " +
          "for approve/reject/override) -- or none is configured on one " +
          "side. Check the token for this action and reload.",
        raw,
      };
    }
    if (error.status === 404) {
      return {
        headline: "Not found",
        nextStep: "It may have been pruned, or the id in the URL is wrong.",
        raw,
      };
    }
    // When the server said why in its own words, that is the next step: a
    // refusal such as "no verify command resolvable: pass -verify-command..."
    // is exactly what the operator needs, and it was once one click away
    // behind Details (found on the live walk, 2026-10-05). A body that is not
    // the server's error shape (a proxy's HTML page) stays behind Details.
    const said = error.serverMessage !== error.body;
    return {
      headline: `Request failed (${error.status})`,
      nextStep: said ? raw : "See details below.",
      raw,
    };
  }

  const raw = rawText(error);
  const lower = raw.toLowerCase();
  if (CONNECTION_MARKERS.some((marker) => lower.includes(marker))) {
    return {
      headline: "Can't reach factoryd",
      nextStep: "Is `factoryd serve` running, and is CORS enabled (see #171)?",
      raw,
    };
  }
  return {
    headline: "Something went wrong",
    nextStep: "See details below, or try again.",
    raw,
  };
}

export interface ErrorCalloutProps {
  readonly error: unknown;
  /**
   * Shows a Retry button when given. Only pass this where the screen has no
   * other visible way to retry. The button is disabled while the returned
   * promise is pending.
   */
  readonly onRetry?: () => void | Promise<void>;
  /** See describeError: pass true from a screen whose call is start-token-gated. */
  readonly startClass?: boolean;
}

/**
 * The shared rendering of an ErrorSummary: headline and next step as the
 * primary text, an optional Retry action, and the raw text demoted into a
 * collapsed "Details" disclosure (omitted when the next step is that text). All text is rendered as text: server and
 * agent text is untrusted.
 */
export function ErrorCallout({ error, onRetry, startClass = false }: ErrorCalloutProps) {
  const summary = describeError(error, { startClass });
  const [pending, setPending] = useState(false);
  const retry = async () => {
    if (onRetry === undefined) return;
    setPending(true);
    try {
      await onRetry();
    } finally {
      setPending(false);
    }
  };
  return (
    <div role="alert" className="flex flex-col items-start gap-1 text-sm">
      <h3 className="text-tone-danger font-semibold">{summary.headline}</h3>
      <p className="text-fg whitespace-pre-line">{summary.nextStep}</p>
      {onRetry !== undefined && (
        <Button size="sm" disabled={pending} onClick={() => void retry()}>
          Retry
        </Button>
      )}
      {/* Nothing to disclose when the next step already is the whole text. */}
      {summary.raw !== summary.nextStep && (
        <details className="w-full">
          <summary className="text-fg-muted cursor-pointer select-none">Details</summary>
          <pre className="bg-surface-sunken text-fg-muted mt-1 overflow-x-auto rounded-md p-2 text-xs whitespace-pre-wrap">
            {summary.raw}
          </pre>
        </details>
      )}
    </div>
  );
}
