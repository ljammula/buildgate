// A request's (or a ticket's) oracle directory: the listing GET
// /requests/{id}/oracle serves, one fetched file, and the MANIFEST.json that
// maps files to spec criteria. An oracle file is code the operator is about
// to approve, so what counts as "shown" is decided here, purely.
import { decodeUtf8Escaping } from "@/domain/textEscape";
import {
  type JsonObject,
  asArray,
  isObject,
  numberOr,
  objectList,
  optString,
  reqString,
} from "@/domain/decode";

/** One file of an oracle directory, as the listing has it (internal/request.OracleFile). */
export interface OracleFileInfo {
  readonly name: string;
  readonly size: number;
  readonly sha256: string;
}

export function decodeOracleFileInfo(o: JsonObject, at: string): OracleFileInfo {
  return {
    name: reqString(o, "name", at),
    size: numberOr(o, "size", at, 0),
    sha256: optString(o, "sha256", at),
  };
}

// Dart's Object.toString for the values a tolerant list may hold, so a
// non-string element is shown rather than rejected.
function textOf(value: unknown): string {
  if (typeof value === "string") return value;
  if (value !== null && typeof value === "object") return JSON.stringify(value);
  return String(value);
}

function problemList(o: JsonObject, at: string): string[] {
  const value = o.problems;
  if (value === undefined || value === null) return [];
  return asArray(value, `${at}.problems`).map(textOf);
}

/**
 * GET /requests/{id}/oracle: the oracle/ files with their hashes, every
 * problem approval would refuse, and the drafting status record.
 */
export interface OracleListing {
  readonly files: readonly OracleFileInfo[];
  readonly problems: readonly string[];
  readonly state: string;
  /** The drafting record's fields; "" when there is no record or the field is not a string. */
  readonly draftStatus: string;
  readonly draftDetail: string;
  readonly proposedCommand: string;
}

export function decodeOracleListing(o: JsonObject, at: string): OracleListing {
  const draft = o.oracle_draft;
  const draftField = (key: string): string => {
    if (!isObject(draft)) return "";
    const value = draft[key];
    return typeof value === "string" ? value : "";
  };
  return {
    files: objectList(o, "files", at, decodeOracleFileInfo),
    problems: problemList(o, at),
    state: optString(o, "state", at),
    draftStatus: draftField("status"),
    draftDetail: draftField("detail"),
    proposedCommand: draftField("proposed_command"),
  };
}

/**
 * One oracle file as fetched. `bytes` is exactly what was received: the
 * approval hash is always over these, never over `text`. `text` is what the
 * operator is shown (see {@link oracleFileContentFromBytes}).
 */
export interface OracleFileContent {
  readonly bytes: Uint8Array;
  readonly text: string;
  /** False when the bytes are not well-formed UTF-8, so `text` carries `\uDC80`-style byte escapes. */
  readonly validUtf8: boolean;
}

/**
 * Builds the fetched content of one oracle file. A byte order mark is kept
 * (it is part of the bytes the operator approves), and invalid bytes become
 * surrogate escapes rather than U+FFFD.
 */
export function oracleFileContentFromBytes(bytes: Uint8Array): OracleFileContent {
  let validUtf8 = true;
  try {
    new TextDecoder("utf-8", { fatal: true, ignoreBOM: true }).decode(bytes);
  } catch {
    validUtf8 = false;
  }
  return { bytes, text: decodeUtf8Escaping(bytes), validUtf8 };
}

/** One MANIFEST.json entry: which criterion an oracle file covers, or why none does. */
export interface OracleManifestEntry {
  readonly criterion: string;
  readonly oracleFile: string | null;
  readonly rationale: string;
  readonly targetPath: string;
  readonly supersedes: readonly string[];
  readonly criterionIndex: number | null;
}

/**
 * Parses MANIFEST.json tolerantly: null when it is not a JSON array (the raw
 * file is still shown), and non-object elements are skipped.
 */
export function parseOracleManifest(text: string): OracleManifestEntry[] | null {
  let decoded: unknown;
  try {
    decoded = JSON.parse(text);
  } catch {
    return null;
  }
  if (!Array.isArray(decoded)) return null;
  const str = (v: unknown): string => (typeof v === "string" ? v : "");
  const entries: OracleManifestEntry[] = [];
  for (const e of decoded as unknown[]) {
    if (!isObject(e)) continue;
    entries.push({
      criterion: str(e.criterion),
      oracleFile: typeof e.oracle_file === "string" ? e.oracle_file : null,
      rationale: str(e.rationale),
      targetPath: str(e.target_path),
      supersedes: Array.isArray(e.supersedes) ? (e.supersedes as unknown[]).map(textOf) : [],
      criterionIndex: Number.isInteger(e.criterion_index) ? (e.criterion_index as number) : null,
    });
  }
  return entries;
}

/**
 * The files that count as "shown": listed, open, and whose fetched content's
 * hash equals the listing's. An approval built from this never carries a hash
 * for bytes the operator did not see.
 *
 * @param listed key -> listed sha256
 * @param opened keys whose tile is open
 * @param contentHashes key -> sha256 of the content fetched for that key
 */
export function shownOracleFiles(
  listed: ReadonlyMap<string, string>,
  opened: ReadonlySet<string>,
  contentHashes: ReadonlyMap<string, string>,
): Map<string, string> {
  const shown = new Map<string, string>();
  for (const [key, sha] of listed) {
    if (opened.has(key) && contentHashes.get(key) === sha) shown.set(key, sha);
  }
  return shown;
}

/**
 * Reconciles fetched content with a fresh listing: the keys whose content is
 * kept. Content whose hash moved or that is no longer listed is forgotten, so
 * it is re-fetched rather than shown stale.
 */
export function keptOracleContentKeys(
  listed: ReadonlyMap<string, string>,
  contentHashes: ReadonlyMap<string, string>,
): string[] {
  return [...contentHashes].filter(([key, sha]) => listed.get(key) === sha).map(([key]) => key);
}

/** The keys that stay open after a fresh listing: only still-listed files. */
export function keptOpenOracleKeys(
  listed: ReadonlyMap<string, string>,
  opened: ReadonlySet<string>,
): string[] {
  return [...opened].filter((key) => listed.has(key));
}

/** The first 12 hex characters of a hash, as a file tile's subtitle shows it. */
export function shortOracleHash(sha256: string): string {
  return sha256.length > 12 ? sha256.slice(0, 12) : sha256;
}

/** A file tile's subtitle: `22 bytes · sha256 0e2df58c76e6`. */
export function oracleFileSubtitle(file: OracleFileInfo): string {
  return `${file.size} bytes · sha256 ${shortOracleHash(file.sha256)}`;
}
