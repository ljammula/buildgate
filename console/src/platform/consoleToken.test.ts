import {
  captureConsoleTokenFromLocation,
  clearStoredConsoleToken,
  getStoredConsoleToken,
  setStoredConsoleToken,
} from "@/platform/consoleToken";
import { captureGateTokenFromLocation } from "@/platform/gateToken";
import { captureStartTokenFromLocation, getStoredStartToken } from "@/platform/startToken";

afterEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
  window.history.replaceState(null, "", "/");
});

test("starts out unset; set, get and clear round-trip in localStorage", () => {
  expect(getStoredConsoleToken()).toBeNull();
  setStoredConsoleToken("host-1");
  expect(getStoredConsoleToken()).toBe("host-1");
  expect(window.localStorage.length).toBe(1);
  expect(window.sessionStorage.length).toBe(0);
  clearStoredConsoleToken();
  expect(getStoredConsoleToken()).toBeNull();
});

test("captures #c= beside the start token and leaves the start token for its own capture", () => {
  window.history.replaceState(null, "", "/requests?x=1#t=start-1&c=host-1");
  expect(captureGateTokenFromLocation()).toBeNull();
  expect(captureConsoleTokenFromLocation()).toBe("host-1");
  expect(window.location.hash).toBe("#t=start-1");
  captureStartTokenFromLocation();
  expect(getStoredStartToken()).toBe("start-1");
  expect(window.location.pathname + window.location.search + window.location.hash).toBe(
    "/requests?x=1",
  );
  // Capturing stores nothing: the session stores it once the server accepts it.
  expect(getStoredConsoleToken()).toBeNull();
});

test("an empty or malformed c= is removed and is no token", () => {
  window.history.replaceState(null, "", "/#c=&t=s");
  expect(captureConsoleTokenFromLocation()).toBeNull();
  expect(window.location.hash).toBe("#t=s");
  window.history.replaceState(null, "", "/#c=%E0%A4%A");
  expect(captureConsoleTokenFromLocation()).toBeNull();
  expect(window.location.hash).toBe("");
});

test('does not match a key merely ending in c, e.g. "abc="', () => {
  window.history.replaceState(null, "", "/#abc=x");
  expect(captureConsoleTokenFromLocation()).toBeNull();
  expect(window.location.hash).toBe("#abc=x");
});
