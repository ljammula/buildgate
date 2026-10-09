import { asObject } from "@/domain/decode";
import { decodeProjectMemory, memoryBudgetText } from "@/domain/memory";
import { readFixtureJson } from "@/test/fixtures";

const at = "GET /projects/{project}/memory";

test("the contract fixture decodes with every field", () => {
  const memory = decodeProjectMemory(asObject(readFixtureJson("api/project-memory.json"), at), at);
  expect(memory.project).toBe("app");
  expect(memory.on).toBe(true);
  expect(memory.offReason).toBe("every-field off_reason");
  expect(memory.inForce).toEqual(["every-field in_force"]);
  expect(memory.sectionError).toBe("every-field section_error");
  expect(memory.candidates).toEqual([
    {
      id: "every-field id",
      line: "every-field line",
      source: "every-field source",
      state: "every-field state",
      seen: 3,
      firstSeenAt: "2026-09-10T09:00:00Z",
      lastSeenAt: "2026-09-10T09:00:00Z",
      requestId: "every-field request_id",
    },
  ]);
  expect(memoryBudgetText(memory)).toBe("3 of 3 lines, 3 of 3 characters");
});

test("a memory that is off with nothing stored decodes to empty lists", () => {
  const memory = decodeProjectMemory(
    {
      project: "app",
      on: false,
      off_reason: "this repository is not listed under memory.repositories in the session config",
      budget_lines: 40,
      budget_chars: 3000,
      used_lines: 0,
      used_chars: 0,
      in_force: [],
      candidates: [],
    },
    at,
  );
  expect(memory.on).toBe(false);
  expect(memory.inForce).toEqual([]);
  expect(memory.candidates).toEqual([]);
  expect(memory.sectionError).toBe("");
});

test("a response without the switch is refused with the route's name", () => {
  expect(() => decodeProjectMemory({ project: "app" }, at)).toThrow(
    /GET \/projects\/\{project\}\/memory/,
  );
});
