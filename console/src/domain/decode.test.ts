import {
  DecodeError,
  asObject,
  decodeList,
  numberOr,
  objectList,
  optBoolean,
  optNumber,
  optObject,
  optString,
  reqString,
  stringList,
  stringMap,
} from "@/domain/decode";

const at = "GET /runs/{id}";

test("a missing required field names the route and the field", () => {
  expect(() => reqString({}, "id", at)).toThrow(
    new DecodeError("GET /runs/{id}.id", "expected a string, got a undefined"),
  );
});

test("a mistyped optional field is drift, not absence", () => {
  expect(() => optString({ branch: 7 }, "branch", at)).toThrow(/GET \/runs\/\{id\}\.branch/);
  expect(() => optBoolean({ stalled: "yes" }, "stalled", at)).toThrow(DecodeError);
});

test("absent and null optional fields take their fallback", () => {
  expect(optString({}, "branch", at)).toBe("");
  expect(optString({ branch: null }, "branch", at, "main")).toBe("main");
  expect(optNumber({}, "tokens", at)).toBeNull();
  expect(optNumber({ tokens: 0 }, "tokens", at)).toBe(0);
  expect(numberOr({}, "round", at, 0)).toBe(0);
  expect(optBoolean({}, "stalled", at)).toBe(false);
  expect(optObject({ diff_stat: null }, "diff_stat", at, (o) => o)).toBeNull();
});

test("Go's null slice and null map decode as empty", () => {
  expect(stringList({ changed_files: null }, "changed_files", at)).toEqual([]);
  expect(objectList({ attempts: null }, "attempts", at, (o) => o)).toEqual([]);
  expect(stringMap({}, "models", at)).toEqual({});
});

test("a list item's error carries its index", () => {
  expect(() =>
    objectList({ attempts: [{ kind: "build" }, { kind: 3 }] }, "attempts", at, (o, itemAt) =>
      reqString(o, "kind", itemAt),
    ),
  ).toThrow(/GET \/runs\/\{id\}\.attempts\[1\]\.kind: expected a string, got a number/);
  expect(() => decodeList([1], "GET /runs", (o) => o)).toThrow(
    /GET \/runs\[0\]: expected an object/,
  );
});

test("asObject refuses arrays and null", () => {
  expect(() => asObject([], at)).toThrow(/got an array/);
  expect(() => asObject(null, at)).toThrow(/got null/);
});
