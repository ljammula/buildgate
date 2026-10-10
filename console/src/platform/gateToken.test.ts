import {
  captureGateTokenFromLocation,
  clearStoredGateToken,
  getStoredGateToken,
  hashWithoutGateToken,
  parseGateTokenFromHash,
  setStoredGateToken,
} from "@/platform/gateToken";
import { captureStartTokenFromLocation, getStoredStartToken } from "@/platform/startToken";

afterEach(() => {
  vi.restoreAllMocks();
  window.sessionStorage.clear();
  window.localStorage.clear();
  window.history.replaceState(null, "", "/");
});

test("parses a bare #gate=<token> fragment, with or without the leading #", () => {
  expect(parseGateTokenFromHash("#gate=abc123")).toBe("abc123");
  expect(parseGateTokenFromHash("gate=abc123")).toBe("abc123");
});

test("decodes a percent-encoded token", () => {
  expect(parseGateTokenFromHash("#gate=a%2Fb%3D")).toBe("a/b=");
});

test("finds gate= beside a start token, in either order", () => {
  expect(parseGateTokenFromHash("#t=start-1&gate=gate-1")).toBe("gate-1");
  expect(parseGateTokenFromHash("#gate=gate-1&t=start-1")).toBe("gate-1");
});

test("returns null for an empty hash, an empty value or no gate component", () => {
  expect(parseGateTokenFromHash("")).toBeNull();
  expect(parseGateTokenFromHash("#")).toBeNull();
  expect(parseGateTokenFromHash("#gate=")).toBeNull();
  expect(parseGateTokenFromHash("#gate")).toBeNull();
  expect(parseGateTokenFromHash("#t=start-1")).toBeNull();
});

test('does not match a key merely ending in gate, e.g. "agate="', () => {
  expect(parseGateTokenFromHash("#agate=abc123")).toBeNull();
});

test("a malformed percent escape is no token, not a crash", () => {
  expect(parseGateTokenFromHash("#gate=%E0%A4%A")).toBeNull();
});

test("hashWithoutGateToken keeps every other component in order", () => {
  expect(hashWithoutGateToken("#gate=g")).toBe("");
  expect(hashWithoutGateToken("#t=s&gate=g")).toBe("#t=s");
  expect(hashWithoutGateToken("#gate=g&t=s")).toBe("#t=s");
  expect(hashWithoutGateToken("#section&gate=g&t=s")).toBe("#section&t=s");
  expect(hashWithoutGateToken("#t=s")).toBe("#t=s");
  expect(hashWithoutGateToken("")).toBe("");
});

test("starts out unset; set, get and clear round-trip", () => {
  expect(getStoredGateToken()).toBeNull();
  setStoredGateToken("gate-1");
  expect(getStoredGateToken()).toBe("gate-1");
  clearStoredGateToken();
  expect(getStoredGateToken()).toBeNull();
});

test("the token is kept in sessionStorage under one key, never localStorage", () => {
  setStoredGateToken("gate-1");
  expect(window.sessionStorage.getItem("factoryGateToken")).toBe("gate-1");
  expect(window.sessionStorage.length).toBe(1);
  expect(window.localStorage.length).toBe(0);
});

test("capture returns the token, strips only its component and stores nothing", () => {
  window.history.replaceState(null, "", "/requests?project=one#gate=gate-1");
  expect(captureGateTokenFromLocation()).toBe("gate-1");
  expect(window.location.pathname).toBe("/requests");
  expect(window.location.search).toBe("?project=one");
  expect(window.location.hash).toBe("");
  expect(window.sessionStorage.length).toBe(0);
  expect(window.localStorage.length).toBe(0);
});

test("capture leaves the rest of the fragment for the start token, in either order", () => {
  for (const hash of ["#t=start-1&gate=gate-1", "#gate=gate-1&t=start-1"]) {
    window.localStorage.clear();
    window.history.replaceState(null, "", `/runs?state=failed${hash}`);
    expect(captureGateTokenFromLocation()).toBe("gate-1");
    expect(window.location.hash).toBe("#t=start-1");
    captureStartTokenFromLocation();
    expect(getStoredStartToken()).toBe("start-1");
    expect(window.location.pathname + window.location.search + window.location.hash).toBe(
      "/runs?state=failed",
    );
  }
});

test("capture strips a malformed or empty gate component and returns null", () => {
  for (const hash of ["#gate=%E0%A4%A", "#gate="]) {
    window.history.replaceState(null, "", `/runs${hash}`);
    expect(captureGateTokenFromLocation()).toBeNull();
    expect(window.location.hash).toBe("");
  }
});

test("capture leaves a fragment with no gate component alone", () => {
  window.history.replaceState(null, "", "/runs#section");
  const replaceState = vi.spyOn(window.history, "replaceState");
  expect(captureGateTokenFromLocation()).toBeNull();
  expect(replaceState).not.toHaveBeenCalled();
  expect(window.location.hash).toBe("#section");
});

test("storage that throws reads as no token and never throws", () => {
  vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => {
    throw new DOMException("denied", "SecurityError");
  });
  vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => {
    throw new DOMException("full", "QuotaExceededError");
  });
  vi.spyOn(Storage.prototype, "removeItem").mockImplementation(() => {
    throw new DOMException("denied", "SecurityError");
  });
  expect(getStoredGateToken()).toBeNull();
  expect(() => {
    setStoredGateToken("gate-1");
    clearStoredGateToken();
  }).not.toThrow();
});

test("a browser that refuses sessionStorage itself reads as no token", () => {
  vi.spyOn(window, "sessionStorage", "get").mockImplementation(() => {
    throw new DOMException("denied", "SecurityError");
  });
  expect(getStoredGateToken()).toBeNull();
  expect(() => {
    setStoredGateToken("gate-1");
    clearStoredGateToken();
  }).not.toThrow();
});
