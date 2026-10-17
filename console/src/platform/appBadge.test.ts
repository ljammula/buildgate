import { setAppBadge } from "@/platform/appBadge";

afterEach(() => {
  vi.unstubAllGlobals();
});

function stubBadging(result: () => Promise<void> = () => Promise.resolve()) {
  const calls: string[] = [];
  vi.stubGlobal("navigator", {
    setAppBadge: (count?: number) => {
      calls.push(`set:${String(count)}`);
      return result();
    },
    clearAppBadge: () => {
      calls.push("clear");
      return result();
    },
  });
  return calls;
}

test("a count is shown on the app icon", () => {
  const calls = stubBadging();
  setAppBadge(3);
  expect(calls).toEqual(["set:3"]);
});

test("zero and an unknown count clear the badge", () => {
  const calls = stubBadging();
  setAppBadge(0);
  setAppBadge(null);
  expect(calls).toEqual(["clear", "clear"]);
});

test("a browser without the API is left alone", () => {
  vi.stubGlobal("navigator", {});
  expect(() => {
    setAppBadge(2);
  }).not.toThrow();
});

test("a refused or throwing badge call is not an error", async () => {
  stubBadging(() => Promise.reject(new Error("NotAllowedError")));
  setAppBadge(2);
  vi.stubGlobal("navigator", {
    setAppBadge: () => {
      throw new Error("refused");
    },
    clearAppBadge: () => Promise.resolve(),
  });
  expect(() => {
    setAppBadge(2);
  }).not.toThrow();
  // The rejected promise is handled, so nothing surfaces as unhandled.
  await Promise.resolve();
});
