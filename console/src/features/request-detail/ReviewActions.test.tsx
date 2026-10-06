import { oracleHint } from "./ReviewActions";

test("the hint counts the oracle files left to open", () => {
  expect(oracleHint(2)).toBe("Open 2 oracle files first");
  expect(oracleHint(1)).toBe("Open 1 oracle file first");
});

test("the hint stays generic while the count is unknown", () => {
  expect(oracleHint(null)).toBe("Open every oracle file first");
});
