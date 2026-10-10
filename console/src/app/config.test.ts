import { resolveHttpConfig } from "@/app/config";

const none = {
  baseUrl: "",
  auth: "",
  start: "",
  override: "",
  read: "",
  storedStartToken: null,
  storedGateToken: null,
};

test("with nothing configured every token is absent", () => {
  expect(resolveHttpConfig(none)).toEqual({
    baseUrl: "",
    readToken: null,
    startToken: null,
    overrideToken: null,
    gateToken: null,
  });
});

test("the stored start token is used when the bundle carries none", () => {
  expect(resolveHttpConfig({ ...none, storedStartToken: "from-link" }).startToken).toBe(
    "from-link",
  );
});

test("a built-in start or auth token wins over the stored one", () => {
  expect(resolveHttpConfig({ ...none, start: "s", storedStartToken: "from-link" }).startToken).toBe(
    "s",
  );
  expect(resolveHttpConfig({ ...none, auth: "a", storedStartToken: "from-link" }).startToken).toBe(
    "a",
  );
});

test("one auth token stands in for the start and override tokens, never the read token", () => {
  expect(resolveHttpConfig({ ...none, auth: "a" })).toEqual({
    baseUrl: "",
    readToken: null,
    startToken: "a",
    overrideToken: "a",
    gateToken: null,
  });
});

test("distinct start and override tokens stay distinct", () => {
  const config = resolveHttpConfig({ ...none, auth: "a", start: "s", override: "o", read: "r" });
  expect(config.startToken).toBe("s");
  expect(config.overrideToken).toBe("o");
  expect(config.readToken).toBe("r");
});

test("the stored start token never becomes the override token", () => {
  expect(resolveHttpConfig({ ...none, storedStartToken: "from-link" }).overrideToken).toBeNull();
});

test("the stored gate token is the gate token and stands in for no other", () => {
  expect(resolveHttpConfig({ ...none, storedGateToken: "from-gate-link" })).toEqual({
    baseUrl: "",
    readToken: null,
    startToken: null,
    overrideToken: null,
    gateToken: "from-gate-link",
  });
});

test("no bundle token becomes the gate token", () => {
  const config = resolveHttpConfig({ ...none, auth: "a", start: "s", override: "o", read: "r" });
  expect(config.gateToken).toBeNull();
});
