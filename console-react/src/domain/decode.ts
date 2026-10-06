// The decoding toolkit every domain decoder is written with. The API's JSON
// is typed by hand here (no code generation), so a decoder states exactly
// which fields the console needs and how it treats each one:
//
//   - a required field that is missing or mistyped throws a DecodeError
//     naming where it was read from (the route, then the field path);
//   - an optional field that is absent or null takes its fallback;
//   - an optional field that is present with the wrong type also throws,
//     because a wrong type is drift between Go and TypeScript, not absence;
//   - unknown fields are ignored.
//
// `at` is the location so far, e.g. "GET /runs/{id}" or
// "GET /runs/{id}.attempts[0]".

export type JsonObject = Record<string, unknown>;

export class DecodeError extends Error {
  readonly at: string;

  constructor(at: string, problem: string) {
    super(`${at}: ${problem}`);
    this.name = "DecodeError";
    this.at = at;
  }
}

function describe(value: unknown): string {
  if (value === null) return "null";
  if (Array.isArray(value)) return "an array";
  return `a ${typeof value}`;
}

function absent(value: unknown): value is null | undefined {
  return value === undefined || value === null;
}

export function isObject(value: unknown): value is JsonObject {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

export function asObject(value: unknown, at: string): JsonObject {
  if (!isObject(value)) throw new DecodeError(at, `expected an object, got ${describe(value)}`);
  return value;
}

export function asArray(value: unknown, at: string): unknown[] {
  if (!Array.isArray(value)) throw new DecodeError(at, `expected an array, got ${describe(value)}`);
  return value;
}

export function asString(value: unknown, at: string): string {
  if (typeof value !== "string")
    throw new DecodeError(at, `expected a string, got ${describe(value)}`);
  return value;
}

export function asNumber(value: unknown, at: string): number {
  if (typeof value !== "number" || !Number.isFinite(value))
    throw new DecodeError(at, `expected a number, got ${describe(value)}`);
  return value;
}

export function asBoolean(value: unknown, at: string): boolean {
  if (typeof value !== "boolean")
    throw new DecodeError(at, `expected a boolean, got ${describe(value)}`);
  return value;
}

export function reqString(o: JsonObject, key: string, at: string): string {
  return asString(o[key], `${at}.${key}`);
}

export function reqNumber(o: JsonObject, key: string, at: string): number {
  return asNumber(o[key], `${at}.${key}`);
}

export function reqBoolean(o: JsonObject, key: string, at: string): boolean {
  return asBoolean(o[key], `${at}.${key}`);
}

export function reqObject<T>(
  o: JsonObject,
  key: string,
  at: string,
  decode: (value: JsonObject, at: string) => T,
): T {
  return decode(asObject(o[key], `${at}.${key}`), `${at}.${key}`);
}

/** An optional string: `fallback` (default "") when absent or null. */
export function optString(o: JsonObject, key: string, at: string, fallback = ""): string {
  const value = o[key];
  return absent(value) ? fallback : asString(value, `${at}.${key}`);
}

/** An optional number: null when absent or null, so "unknown" stays apart from 0. */
export function optNumber(o: JsonObject, key: string, at: string): number | null {
  const value = o[key];
  return absent(value) ? null : asNumber(value, `${at}.${key}`);
}

/** An optional number with a fallback, for a field whose zero value Go omits. */
export function numberOr(o: JsonObject, key: string, at: string, fallback: number): number {
  return optNumber(o, key, at) ?? fallback;
}

/** An optional boolean: `fallback` (default false) when absent or null. */
export function optBoolean(o: JsonObject, key: string, at: string, fallback = false): boolean {
  const value = o[key];
  return absent(value) ? fallback : asBoolean(value, `${at}.${key}`);
}

/** An optional nested object: null when absent or null. */
export function optObject<T>(
  o: JsonObject,
  key: string,
  at: string,
  decode: (value: JsonObject, at: string) => T,
): T | null {
  const value = o[key];
  return absent(value) ? null : decode(asObject(value, `${at}.${key}`), `${at}.${key}`);
}

/**
 * A list of objects: empty when absent or null. Go encodes a nil slice as
 * null, so null is an ordinary empty list, not an error.
 */
export function objectList<T>(
  o: JsonObject,
  key: string,
  at: string,
  decode: (value: JsonObject, at: string) => T,
): T[] {
  const value = o[key];
  if (absent(value)) return [];
  return asArray(value, `${at}.${key}`).map((item, index) => {
    const itemAt = `${at}.${key}[${index}]`;
    return decode(asObject(item, itemAt), itemAt);
  });
}

/** A list of strings: empty when absent or null. */
export function stringList(o: JsonObject, key: string, at: string): string[] {
  const value = o[key];
  if (absent(value)) return [];
  return asArray(value, `${at}.${key}`).map((item, index) =>
    asString(item, `${at}.${key}[${index}]`),
  );
}

/** A string-to-string map: empty when absent or null. */
export function stringMap(o: JsonObject, key: string, at: string): Record<string, string> {
  const value = o[key];
  if (absent(value)) return {};
  const out: Record<string, string> = {};
  for (const [name, item] of Object.entries(asObject(value, `${at}.${key}`))) {
    out[name] = asString(item, `${at}.${key}.${name}`);
  }
  return out;
}

/** A string-to-number map: empty when absent or null. */
export function numberMap(o: JsonObject, key: string, at: string): Record<string, number> {
  const value = o[key];
  if (absent(value)) return {};
  const out: Record<string, number> = {};
  for (const [name, item] of Object.entries(asObject(value, `${at}.${key}`))) {
    out[name] = asNumber(item, `${at}.${key}.${name}`);
  }
  return out;
}

/** Decodes a top-level array response. */
export function decodeList<T>(
  value: unknown,
  at: string,
  decode: (value: JsonObject, at: string) => T,
): T[] {
  return asArray(value, at).map((item, index) => {
    const itemAt = `${at}[${index}]`;
    return decode(asObject(item, itemAt), itemAt);
  });
}
