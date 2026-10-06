#!/usr/bin/env node
// The test port map: every test of the Flutter console is one of
//   - ported to a Vitest test with the same name;
//   - listed under "covered" in port-map.json, naming the Vitest test (its
//     own title, which must exist) that covers it under another name, e.g.
//     one case of a table test;
//   - listed under "dropped" with the reason it has no counterpart.
// This script fails on a Flutter test that is none of these, and on a
// "covered" entry whose Vitest test does not exist. Keys are
// "<dart test file> :: <full dart test name>".
//
//   node scripts/port-map.mjs            list what is unmapped, exit 1 if any
//   node scripts/port-map.mjs --summary  counts per Dart test file only
//
// It runs the Flutter tests once (their machine-readable reporter is the
// only exact source of test names) and lists the Vitest tests without
// running them. It is deleted with the Flutter console at the cutover.
import { execFileSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { dirname, relative, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const reactRoot = resolve(here, "..");
const flutterRoot = resolve(reactRoot, "../console");

function run(command, args, cwd) {
  try {
    return execFileSync(command, args, { cwd, encoding: "utf8", maxBuffer: 256 * 1024 * 1024 });
  } catch (error) {
    // A failing test still prints the full listing; the names are what matter here.
    if (typeof error.stdout === "string" && error.stdout !== "") return error.stdout;
    throw error;
  }
}

function dartTests() {
  const out = run("flutter", ["test", "--machine"], flutterRoot);
  const suites = new Map();
  const tests = [];
  for (const line of out.split("\n")) {
    if (!line.startsWith("{")) continue;
    let event;
    try {
      event = JSON.parse(line);
    } catch {
      continue;
    }
    if (event.type === "suite") suites.set(event.suite.id, event.suite.path);
    if (event.type === "testStart" && !event.test.name.startsWith("loading ")) {
      const path = suites.get(event.test.suiteID) ?? "";
      tests.push({ file: relative(flutterRoot, path), name: event.test.name });
    }
  }
  return tests;
}

function vitestTitles() {
  const out = run("npx", ["vitest", "list", "--json"], reactRoot);
  const listed = JSON.parse(out.slice(out.indexOf("[")));
  // "describe > nested > title": the leaf is what a ported test keeps.
  return new Set(listed.map((test) => test.name.split(" > ").at(-1)));
}

const normalize = (name) => name.replace(/\s+/g, " ").trim();

const portMap = JSON.parse(readFileSync(resolve(reactRoot, "port-map.json"), "utf8"));
const dropped = portMap.dropped ?? {};
const covered = portMap.covered ?? {};
const titles = new Set([...vitestTitles()].map(normalize));
const titleList = [...titles];

function ported(name) {
  const full = normalize(name);
  if (titles.has(full)) return true;
  // A Dart name is "group test"; the ported test keeps the test's own name.
  return titleList.some((title) => title.length >= 12 && full.endsWith(` ${title}`));
}

const byFile = new Map();
for (const test of dartTests()) {
  const entry = byFile.get(test.file) ?? { ported: 0, dropped: 0, unmapped: [] };
  const key = `${test.file} :: ${normalize(test.name)}`;
  if (ported(test.name)) entry.ported += 1;
  else if (covered[key] !== undefined) {
    if (titles.has(normalize(covered[key]))) entry.ported += 1;
    else
      entry.unmapped.push(
        `${normalize(test.name)}  [covered by a test that does not exist: ${covered[key]}]`,
      );
  } else if (dropped[key]) entry.dropped += 1;
  else entry.unmapped.push(normalize(test.name));
  byFile.set(test.file, entry);
}

const summaryOnly = process.argv.includes("--summary");
let total = 0;
let unmapped = 0;
for (const [file, entry] of [...byFile].sort()) {
  const count = entry.ported + entry.dropped + entry.unmapped.length;
  total += count;
  unmapped += entry.unmapped.length;
  console.log(
    `${file}: ${entry.ported} ported, ${entry.dropped} dropped, ${entry.unmapped.length} unmapped of ${count}`,
  );
  if (!summaryOnly) for (const name of entry.unmapped) console.log(`    ${name}`);
}
console.log(`\n${total - unmapped} of ${total} Flutter tests are mapped; ${unmapped} are not.`);
process.exit(unmapped === 0 ? 0 : 1);
