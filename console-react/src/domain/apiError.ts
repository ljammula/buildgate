/**
 * A non-2xx response from the API. `body` is the raw response text; the
 * getters read the server's own JSON shapes out of it without ever throwing
 * (this is an error's display path).
 */
export class ApiError extends Error {
  readonly status: number;
  readonly body: string;

  constructor(status: number, body: string) {
    super(`Run API request failed (${status}): ${body}`);
    this.name = "ApiError";
    this.status = status;
    this.body = body;
  }

  private field(name: string): string | null {
    try {
      const decoded: unknown = JSON.parse(this.body);
      if (typeof decoded === "object" && decoded !== null && !Array.isArray(decoded)) {
        const value = (decoded as Record<string, unknown>)[name];
        if (typeof value === "string") return value;
      }
    } catch {
      // Not JSON: nothing to extract.
    }
    return null;
  }

  /**
   * The server's `{"error": "..."}` text (internal/api's writeError), so the
   * operator reads the diagnostic alone rather than a JSON envelope. Falls
   * back to the raw body when it is not that shape.
   */
  get serverMessage(): string {
    return this.field("error") ?? this.body;
  }

  /**
   * A 409 edit conflict's `current_sha256` (the spec, ticket and oracle
   * command PUT routes, when `base_sha256` did not match); null for any
   * other status or body, so `currentSha256 !== null` tells a real edit
   * conflict from an unrelated 409.
   */
  get currentSha256(): string | null {
    return this.status === 409 ? this.field("current_sha256") : null;
  }

  /** A 503 is the server failing to check, not refusing: trying again may work. */
  get isRetryable(): boolean {
    return this.status === 503;
  }

  /**
   * `serverMessage` split on the " | " a project check joins its
   * per-artifact failures with, so each failing check renders on its own
   * line. A message with no delimiter is its own single part.
   */
  get messageParts(): string[] {
    return this.serverMessage.split(" | ");
  }

  /** A 4xx will not succeed on retry (a rotated token, a pruned run). */
  get isPermanent(): boolean {
    return this.status >= 400 && this.status < 500;
  }
}
