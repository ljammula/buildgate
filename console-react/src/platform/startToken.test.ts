import {
  captureStartTokenFromLocation,
  getStoredStartToken,
  parseStartTokenFromHash,
  setStoredStartToken,
} from "@/platform/startToken";

afterEach(() => {
  window.localStorage.clear();
  window.history.replaceState(null, "", "/");
});

test("parses a bare #t=<token> fragment", () => {
  expect(parseStartTokenFromHash("#t=abc123")).toBe("abc123");
});

test("accepts the fragment with or without a leading #", () => {
  expect(parseStartTokenFromHash("t=abc123")).toBe("abc123");
});

test("decodes a percent-encoded token", () => {
  expect(parseStartTokenFromHash("#t=a%2Fb%3D")).toBe("a/b=");
});

test("finds t= among other &-joined components", () => {
  expect(parseStartTokenFromHash("#other=1&t=abc123")).toBe("abc123");
  expect(parseStartTokenFromHash("#t=abc123&other=1")).toBe("abc123");
});

test("returns null for an empty hash", () => {
  expect(parseStartTokenFromHash("")).toBeNull();
  expect(parseStartTokenFromHash("#")).toBeNull();
});

test("returns null when there is no t= component", () => {
  expect(parseStartTokenFromHash("#/requests/req-1")).toBeNull();
  expect(parseStartTokenFromHash("#other=1")).toBeNull();
});

test("returns null for an empty token value", () => {
  expect(parseStartTokenFromHash("#t=")).toBeNull();
});

test('does not match a key merely ending in t, e.g. "at="', () => {
  expect(parseStartTokenFromHash("#at=abc123")).toBeNull();
});

test("starts out unset", () => {
  expect(getStoredStartToken()).toBeNull();
});

test("set/get round-trips", () => {
  setStoredStartToken("tok-1");
  expect(getStoredStartToken()).toBe("tok-1");
});

test("captureStartTokenFromLocation stores and strips the fragment", () => {
  window.history.replaceState(null, "", "/requests?project=one#t=tok-1");
  captureStartTokenFromLocation();
  expect(getStoredStartToken()).toBe("tok-1");
  expect(window.location.pathname).toBe("/requests");
  expect(window.location.search).toBe("?project=one");
  expect(window.location.hash).toBe("");
});

test("a malformed percent escape is no token, not a crash", () => {
  expect(parseStartTokenFromHash("#t=%E0%A4%A")).toBeNull();
});
