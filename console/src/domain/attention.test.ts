import { attend, notificationContent } from "@/domain/attention";
import { type RequestSummary, decodeRequestSummary } from "@/domain/request";
import { requestJson } from "@/test/requestFixtures";

function request(over: Record<string, unknown> = {}): RequestSummary {
  return decodeRequestSummary(
    { ...requestJson({ id: "req-1", state: "spec_review", title: "Add a coupon field" }), ...over },
    "test",
  );
}

const t1 = "2026-10-10T09:00:00.100000000Z";
const t3 = "2026-10-10T09:05:00Z";
// A tab that started looking after every time used below.
const later = Date.parse("2026-10-10T12:00:00Z");

test("the first sight of a request records its time and raises nothing", () => {
  const out = attend(new Map(), request({ last_notified_at: t1 }), later);
  expect(out.raise).toBe(false);
  expect(out.seen.get("req-1")).toBe(t1);
});

test("the first sight of a request with no time records the empty time", () => {
  const out = attend(new Map(), request(), later);
  expect(out.raise).toBe(false);
  expect(out.seen.get("req-1")).toBe("");
});

test("a later time raises once, and the same time again does not", () => {
  const first = attend(new Map([["req-1", t1]]), request({ last_notified_at: t3 }), later);
  expect(first.raise).toBe(true);
  expect(first.seen.get("req-1")).toBe(t3);
  expect(attend(first.seen, request({ last_notified_at: t3 }), later).raise).toBe(false);
});

test("times are compared as instants, whatever their offset", () => {
  const seen = new Map([["req-1", "2026-10-10T10:00:00+01:00"]]);
  expect(attend(seen, request({ last_notified_at: "2026-10-10T09:00:00Z" }), later).raise).toBe(
    false,
  );
  expect(attend(seen, request({ last_notified_at: "2026-10-10T09:00:01Z" }), later).raise).toBe(
    true,
  );
});

test("an earlier time raises nothing and leaves the record", () => {
  const seen = new Map([["req-1", t3]]);
  const out = attend(seen, request({ last_notified_at: t1 }), later);
  expect(out.raise).toBe(false);
  expect(out.seen).toBe(seen);
});

test("an empty time keeps the recorded one, so clear then set later compares against the old", () => {
  const cleared = attend(new Map([["req-1", t1]]), request(), later);
  expect(cleared.raise).toBe(false);
  expect(cleared.seen.get("req-1")).toBe(t1);
  expect(attend(cleared.seen, request({ last_notified_at: t3 }), later).raise).toBe(true);
  expect(attend(cleared.seen, request({ last_notified_at: t1 }), later).raise).toBe(false);
});

test("a time that cannot be parsed raises nothing", () => {
  const seen = new Map([["req-1", t1]]);
  expect(attend(seen, request({ last_notified_at: "yesterday" }), later).raise).toBe(false);
});

test("a first notification for a request seen without one raises", () => {
  expect(attend(new Map([["req-1", ""]]), request({ last_notified_at: t1 }), later).raise).toBe(
    true,
  );
});

test("the input map is never changed", () => {
  const seen = new Map([["req-1", t1]]);
  attend(seen, request({ last_notified_at: t3 }), later);
  expect(seen.get("req-1")).toBe(t1);
});

test("the content is the ask, the project and the title, tagged by the notification it is", () => {
  expect(
    notificationContent(
      request({
        last_ask: "Spec ready for your review",
        project: "checkouts",
        last_notified_at: t1,
      }),
    ),
  ).toEqual({
    title: "Spec ready for your review",
    body: "checkouts: Add a coupon field",
    tag: `req-1:${t1}`,
  });
});

test("with no ask or project the title falls back and the body is the request title", () => {
  expect(notificationContent(request({ project: "" }))).toEqual({
    title: "Buildgate: a request needs you",
    body: "Add a coupon field",
    tag: "req-1:",
  });
});

test("a request first seen with a notification sent after this tab started looking raises one", () => {
  const before = Date.parse(t1) - 1;
  const out = attend(new Map(), request({ last_notified_at: t1 }), before);
  expect(out.raise).toBe(true);
  expect(attend(out.seen, request({ last_notified_at: t1 }), before).raise).toBe(false);
});

test("a request first seen with no time, or one that does not parse, raises nothing", () => {
  expect(attend(new Map(), request(), 0).raise).toBe(false);
  expect(attend(new Map(), request({ last_notified_at: "yesterday" }), 0).raise).toBe(false);
});
