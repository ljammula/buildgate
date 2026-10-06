import { ApiError } from "@/domain/apiError";

test("serverMessage extracts the server's error field", () => {
  expect(new ApiError(403, '{"error": "read endpoint is not authorized"}').serverMessage).toBe(
    "read endpoint is not authorized",
  );
});

test("serverMessage falls back to the raw body", () => {
  expect(new ApiError(502, "<html>bad gateway</html>").serverMessage).toBe(
    "<html>bad gateway</html>",
  );
  expect(new ApiError(500, '{"error": 7}').serverMessage).toBe('{"error": 7}');
  expect(new ApiError(500, "[1]").serverMessage).toBe("[1]");
});

test("currentSha256 is read only from a 409", () => {
  const body = '{"error": "stale", "current_sha256": "abc"}';
  expect(new ApiError(409, body).currentSha256).toBe("abc");
  expect(new ApiError(400, body).currentSha256).toBeNull();
  expect(new ApiError(409, '{"error": "busy"}').currentSha256).toBeNull();
  expect(new ApiError(409, "not json").currentSha256).toBeNull();
});

test("messageParts splits a project check's joined failures", () => {
  expect(new ApiError(400, '{"error": "spec not frozen | no tickets"}').messageParts).toEqual([
    "spec not frozen",
    "no tickets",
  ]);
  expect(new ApiError(400, '{"error": "one"}').messageParts).toEqual(["one"]);
});

test("only a 503 is retryable, and a 4xx is permanent", () => {
  expect(new ApiError(503, "").isRetryable).toBe(true);
  expect(new ApiError(500, "").isRetryable).toBe(false);
  expect(new ApiError(404, "").isPermanent).toBe(true);
  expect(new ApiError(503, "").isPermanent).toBe(false);
});
