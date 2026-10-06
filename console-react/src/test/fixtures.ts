import { readFileSync } from "node:fs";
import { resolve } from "node:path";

// The fixtures are shared with the Go tests (internal/api's
// TestConsoleContractFixtures writes api/, and the golden vectors under
// vectors/ are read by Go tests too), so they are read from disk by path,
// never copied. Vitest runs from this package's root.
const fixtureRoot = resolve(process.cwd(), "../console/test/fixtures");

export function fixturePath(relative: string): string {
  return resolve(fixtureRoot, relative);
}

export function readFixtureText(relative: string): string {
  return readFileSync(fixturePath(relative), "utf8");
}

export function readFixtureBytes(relative: string): Uint8Array {
  return new Uint8Array(readFileSync(fixturePath(relative)));
}

export function readFixtureJson(relative: string): unknown {
  return JSON.parse(readFixtureText(relative)) as unknown;
}

/** One entry of api/index.json: which route a fixture file was served by. */
export interface ApiFixture {
  pattern: string;
  path: string;
  file: string;
  status: number;
  content_type: string;
}

export function apiFixtures(): ApiFixture[] {
  return readFixtureJson("api/index.json") as ApiFixture[];
}
