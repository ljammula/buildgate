// A request's oracle files: the listing, one file's bytes, and the one file
// the console may edit.
import type { Http } from "@/api/http";
import { asObject } from "@/domain/decode";
import {
  type OracleFileContent,
  type OracleListing,
  decodeOracleListing,
  oracleFileContentFromBytes,
} from "@/domain/oracle";
import { sha256 } from "@noble/hashes/sha2.js";
import { bytesToHex } from "@noble/hashes/utils.js";

const requestPath = (id: string) => `/requests/${encodeURIComponent(id)}`;

/** One oracle file as fetched, with the hash of the bytes as received. */
export interface FetchedOracleFile {
  readonly content: OracleFileContent;
  /** Lowercase hex SHA-256 over `content.bytes`: what an approval is bound to. */
  readonly sha256: string;
}

/**
 * GET /requests/{id}/oracle: the request-level oracle/ files with sha256,
 * every problem approval would refuse, and the drafting status. Read token.
 */
export async function getRequestOracle(
  http: Http,
  id: string,
  signal?: AbortSignal,
): Promise<OracleListing> {
  const at = "GET /requests/{id}/oracle";
  return decodeOracleListing(
    asObject(await http.getJson(`${requestPath(id)}/oracle`, "read", signal), at),
    at,
  );
}

// The hash is computed over the bytes received, so an approval can be bound
// to exactly what was displayed: the bytes are never decoded and re-encoded
// first.
async function getOracleFile(
  http: Http,
  path: string,
  signal?: AbortSignal,
): Promise<FetchedOracleFile> {
  const bytes = await http.getBytes(path, "read", signal);
  return { content: oracleFileContentFromBytes(bytes), sha256: bytesToHex(sha256(bytes)) };
}

/**
 * GET /requests/{id}/oracle/{name}: one oracle file. The returned hash is
 * computed over the bytes received, so an approval can be bound to exactly
 * what was displayed.
 */
export function getRequestOracleFile(
  http: Http,
  id: string,
  name: string,
  signal?: AbortSignal,
): Promise<FetchedOracleFile> {
  return getOracleFile(http, `${requestPath(id)}/oracle/${encodeURIComponent(name)}`, signal);
}

/**
 * GET /requests/{id}/tickets/{n}/oracle: ticket `n`'s materialized
 * `<NNN>.oracle/` files (the ones plan approval pins), plan_review only.
 */
export async function getRequestTicketOracle(
  http: Http,
  id: string,
  n: number,
  signal?: AbortSignal,
): Promise<OracleListing> {
  const at = "GET /requests/{id}/tickets/{n}/oracle";
  return decodeOracleListing(
    asObject(await http.getJson(`${requestPath(id)}/tickets/${n}/oracle`, "read", signal), at),
    at,
  );
}

/** GET /requests/{id}/tickets/{n}/oracle/{name}: one such file. */
export function getRequestTicketOracleFile(
  http: Http,
  id: string,
  n: number,
  name: string,
  signal?: AbortSignal,
): Promise<FetchedOracleFile> {
  return getOracleFile(
    http,
    `${requestPath(id)}/tickets/${n}/oracle/${encodeURIComponent(name)}`,
    signal,
  );
}

export interface PutOracleRunCommandOptions {
  /** As `baseSha256` of updateRequestSpec: a changed file is refused with a 409 carrying `current_sha256`. */
  readonly baseSha256?: string | null;
}

/**
 * PUT /requests/{id}/oracle/RUN_COMMAND.txt: the one oracle file the
 * console may edit, and only at oracle_review. A 422 carries the validation
 * reason in ApiError.serverMessage. A request write (token kind "gate"). See
 * updateRequestSpec (api/requests) for `baseSha256` and 409 handling.
 */
export async function putRequestOracleRunCommand(
  http: Http,
  id: string,
  content: string,
  options: PutOracleRunCommandOptions = {},
  signal?: AbortSignal,
): Promise<void> {
  const body = {
    content,
    ...(options.baseSha256 != null ? { base_sha256: options.baseSha256 } : {}),
  };
  await http.sendJson("PUT", `${requestPath(id)}/oracle/RUN_COMMAND.txt`, "gate", body, signal);
}
