import { getOperatorName, setOperatorName } from "@/platform/operatorIdentity";

afterEach(() => {
  window.localStorage.clear();
});

test("starts unset", () => {
  expect(getOperatorName()).toBeNull();
});

test("set/get round-trips", () => {
  setOperatorName("Kanna");
  expect(getOperatorName()).toBe("Kanna");
});

test("updates an existing name", () => {
  setOperatorName("First");
  setOperatorName("Second");
  expect(getOperatorName()).toBe("Second");
});

test("uses the exact storage key", () => {
  window.localStorage.setItem("factoryOperatorNameOther", "wrong");
  expect(getOperatorName()).toBeNull();
});
