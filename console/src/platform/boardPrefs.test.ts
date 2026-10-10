import {
  getCollapsedLanes,
  getStoredBoardView,
  setCollapsedLanes,
  setStoredBoardView,
} from "@/platform/boardPrefs";

afterEach(() => {
  vi.restoreAllMocks();
  window.localStorage.clear();
});

test("the view starts unset and stores either choice", () => {
  expect(getStoredBoardView()).toBeNull();
  setStoredBoardView("list");
  expect(getStoredBoardView()).toBe("list");
  setStoredBoardView("board");
  expect(getStoredBoardView()).toBe("board");
});

test("a stored view that is not one of the two is ignored", () => {
  window.localStorage.setItem("factoryBoardView", "calendar");
  expect(getStoredBoardView()).toBeNull();
});

test("collapsed lanes round-trip, sorted", () => {
  expect(getCollapsedLanes()).toEqual([]);
  setCollapsedLanes(["web", "api"]);
  expect(window.localStorage.getItem("factoryBoardCollapsedLanes")).toBe('["api","web"]');
  expect(getCollapsedLanes()).toEqual(["api", "web"]);
});

test("unreadable stored lanes read as none collapsed", () => {
  for (const stored of ["not json", '{"api":true}', "null"]) {
    window.localStorage.setItem("factoryBoardCollapsedLanes", stored);
    expect(getCollapsedLanes()).toEqual([]);
  }
  window.localStorage.setItem("factoryBoardCollapsedLanes", '["api", 3, null]');
  expect(getCollapsedLanes()).toEqual(["api"]);
});

test("storage that throws reads as nothing stored and never throws", () => {
  vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => {
    throw new DOMException("denied", "SecurityError");
  });
  vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => {
    throw new DOMException("full", "QuotaExceededError");
  });
  expect(getStoredBoardView()).toBeNull();
  expect(getCollapsedLanes()).toEqual([]);
  expect(() => {
    setStoredBoardView("list");
    setCollapsedLanes(["api"]);
  }).not.toThrow();
});

test("a browser that refuses localStorage itself reads as nothing stored", () => {
  vi.spyOn(window, "localStorage", "get").mockImplementation(() => {
    throw new DOMException("denied", "SecurityError");
  });
  expect(getStoredBoardView()).toBeNull();
  expect(getCollapsedLanes()).toEqual([]);
  expect(() => {
    setStoredBoardView("board");
    setCollapsedLanes([]);
  }).not.toThrow();
});
