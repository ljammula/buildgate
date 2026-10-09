import { asObject } from "@/domain/decode";
import { decodeSavedPrompts } from "@/domain/savedPrompt";
import { readFixtureJson } from "@/test/fixtures";

const at = "GET /runs/{id}/prompts";

test("GET /runs/{id}/prompts decodes", () => {
  expect(decodeSavedPrompts(asObject(readFixtureJson("api/run-prompts.json"), at), at)).toEqual([
    { name: "build-round-1", attempt: "build-1", bytes: 114, time: "2026-09-10T09:32:00Z" },
    { name: "build-round-2", attempt: "build-1", bytes: 83, time: "2026-09-10T09:33:00Z" },
  ]);
});

test("a run with no prompts decodes to an empty list", () => {
  expect(
    decodeSavedPrompts(asObject(readFixtureJson("api/run-prompts-none.json"), at), at),
  ).toEqual([]);
  expect(decodeSavedPrompts({}, at)).toEqual([]);
});

test("a prompt without a name is refused, naming the route", () => {
  expect(() =>
    decodeSavedPrompts({ prompts: [{ attempt: "build-1", bytes: 1, time: "t" }] }, at),
  ).toThrow(/GET \/runs\/\{id\}\/prompts/);
});
