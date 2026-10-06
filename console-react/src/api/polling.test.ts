import * as polling from "@/api/polling";

describe("polling constants", () => {
  test("every interval and threshold is a positive number", () => {
    for (const [name, value] of Object.entries(polling)) {
      expect(value, name).toBeGreaterThan(0);
    }
  });

  test("the backoff starts below its ceiling", () => {
    expect(polling.defaultInitialBackoffMs).toBeLessThan(polling.defaultMaxBackoffMs);
  });
});
